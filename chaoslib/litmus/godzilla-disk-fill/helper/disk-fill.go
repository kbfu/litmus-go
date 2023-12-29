package helper

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/litmuschaos/litmus-go/pkg/clients"
	experimentTypes "github.com/litmuschaos/litmus-go/pkg/generic/disk-fill/types"
	"github.com/litmuschaos/litmus-go/pkg/log"
	"github.com/litmuschaos/litmus-go/pkg/result"
	"github.com/litmuschaos/litmus-go/pkg/types"
	"github.com/litmuschaos/litmus-go/pkg/utils/common"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"io"
	coreV1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var inject, abort chan os.Signal

// Helper injects the disk-fill chaos
func Helper(clients clients.ClientSets) {

	experimentsDetails := experimentTypes.ExperimentDetails{}
	eventsDetails := types.EventDetails{}
	chaosDetails := types.ChaosDetails{}
	resultDetails := types.ResultDetails{}

	// inject channel is used to transmit signal notifications.
	inject = make(chan os.Signal, 1)
	// Catch and relay certain signal(s) to inject channel.
	signal.Notify(inject, os.Interrupt, syscall.SIGTERM)

	// abort channel is used to transmit signal notifications.
	abort = make(chan os.Signal, 1)
	// Catch and relay certain signal(s) to abort channel.
	signal.Notify(abort, os.Interrupt, syscall.SIGTERM)

	//Fetching all the ENV passed in the helper pod
	log.Info("[PreReq]: Getting the ENV variables")
	getENV(&experimentsDetails)

	// Intialise the chaos attributes
	types.InitialiseChaosVariables(&chaosDetails)

	// Intialise Chaos Result Parameters
	types.SetResultAttributes(&resultDetails, chaosDetails)

	// Set the chaos result uid
	result.SetResultUID(&resultDetails, clients, &chaosDetails)

	if err := diskFill(&experimentsDetails, clients, &eventsDetails, &chaosDetails, &resultDetails); err != nil {
		log.Fatalf("helper pod failed, err: %v", err)
	}
}

// diskFill contains steps to inject disk-fill chaos
func diskFill(experimentsDetails *experimentTypes.ExperimentDetails, clients clients.ClientSets, eventsDetails *types.EventDetails, chaosDetails *types.ChaosDetails, resultDetails *types.ResultDetails) error {

	// Derive the container id of the target container
	containerID, err := common.GetContainerID(experimentsDetails.AppNS, experimentsDetails.TargetPods, experimentsDetails.TargetContainer, clients)
	if err != nil {
		return err
	}
	targetPID, err := common.GetPID(experimentsDetails.ContainerRuntime, containerID, experimentsDetails.SocketPath)
	if err != nil {
		return err
	}
	cgroupManager, err, groupPath := getCGroupManager(targetPID, containerID)
	if err != nil {
		return errors.Errorf("fail to get the cgroup manager, err: %v", err)
	}

	command := fmt.Sprintf("pause nsexec -m /proc/%d/ns/mnt -c /proc/%d/ns/cgroup -i /proc/%d/ns/ipc -n /proc/%d/ns/net -p /proc/%d/ns/pid -l",
		targetPID, targetPID, targetPID, targetPID, targetPID) + " -- " +
		fmt.Sprintf("dd if=/dev/urandom of=%s/test.chaos bs=%v count=1", experimentsDetails.FillPath, experimentsDetails.FileSize)
	log.Infof("[Info]: starting process: %v", command)

	cmd := exec.Command("/bin/bash", "-c", command)
	// enables the process group id
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err = cmd.Start()
	if err != nil {
		return errors.Errorf("fail to start the disk fill process %v, err: %v", command, err)
	}
	// watching for the abort signal and revert the chaos if an abort signal is received
	go abortWatcher(targetPID, experimentsDetails.FillPath)

	// add the process to the cgroup of target container
	if err = addProcessToCgroup(cmd.Process.Pid, cgroupManager, groupPath); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			return errors.Errorf("failed to kill %v process, err: %v", cmd.Process.Pid, killErr)
		}
		return errors.Errorf("fail to add the process into target container cgroup, err: %v", err)
	}

	log.Info("[Info]: Sending signal to resume the process")
	// wait for the process to start before sending the resume signal
	// TODO: need a dynamic way to check the start of the process
	time.Sleep(700 * time.Millisecond)

	// remove pause and resume or start the process
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		return errors.Errorf("fail to remove pause and start the process: %v", err)
	}

	log.Info("[Wait]: Waiting for chaos completion")
	// channel to check the completion of the process
	done := make(chan error)
	go func() {
		output, _ := cmd.CombinedOutput()
		log.Infof("output: %s", string(output))
		done <- cmd.Wait()
	}()

	// check the timeout for the command
	// Note: timeout will occur when process didn't complete even after 10s of chaos duration
	timeout := time.After((time.Duration(experimentsDetails.ChaosDuration) + 30) * time.Second)
	start := time.Now().Unix()
	end := int(start) + experimentsDetails.ChaosDuration

	w, err := clients.KubeClient.CoreV1().Pods(experimentsDetails.AppNS).Watch(context.Background(), v1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", experimentsDetails.TargetPods),
	})
	if err != nil {
		log.Errorf("pod not found: %v", err)
		return err
	}
	for {
		select {
		case <-timeout:
			// the process gets timeout before completion
			log.Infof("[Chaos] The process is not yet completed after the chaos duration of %vs", experimentsDetails.ChaosDuration+30)
			log.Info("[Timeout]: Removing the file generated by chaos")
			if err = removeFile(targetPID, experimentsDetails.FillPath); err != nil {
				return err
			}
			w.Stop()
			return nil
		case event := <-w.ResultChan():
			log.Info("waiting for pod up and running")
			if end < int(time.Now().Unix()) {
				w.Stop()
				break
			}
			pod, ok := event.Object.(*coreV1.Pod)
			if !ok {
				continue
			}
			if event.Type == watch.Modified {
				if pod.Status.Phase == coreV1.PodRunning {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == coreV1.ContainersReady && condition.Status == coreV1.ConditionTrue {
							experimentsDetails.ChaosDuration = end - int(time.Now().Unix())
							log.Infof("still got %v seconds to go, run again", experimentsDetails.ChaosDuration)
							w.Stop()
							return diskFill(experimentsDetails, clients, eventsDetails, chaosDetails, resultDetails)
						}
					}
				}
			}
		}
	}
}

func removeFile(pid int, path string) error {
	command := fmt.Sprintf("nsexec -m /proc/%d/ns/mnt -c /proc/%d/ns/cgroup -i /proc/%d/ns/ipc -n /proc/%d/ns/net -p /proc/%d/ns/pid -l",
		pid, pid, pid, pid, pid) + " -- " +
		fmt.Sprintf("rm -rf %s/test.chaos", path)
	log.Infof("[Info]: starting process: %v", command)

	cmd := exec.Command("/bin/bash", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logrus.Error(string(out))
		return err
	}
	return nil
}

// abortWatcher continuously watch for the abort signals
func abortWatcher(targetPID int, path string) {

	<-abort

	log.Info("[Chaos]: Killing process started because of terminated signal received")
	log.Info("[Abort]: Chaos Revert Started")
	// retry thrice for the chaos revert
	retry := 3
	for retry > 0 {
		if err := removeFile(targetPID, path); err != nil {
			log.Errorf("unable to revert, err :%v", err)
		}
		retry--
		time.Sleep(1 * time.Second)
	}
	log.Info("[Abort]: Chaos Revert Completed")
	os.Exit(1)
}

// parseCgroupFromReader will parse the cgroup file from the reader
func parseCgroupFromReader(r io.Reader) (map[string]string, error) {
	var (
		cgroups = make(map[string]string)
		s       = bufio.NewScanner(r)
	)
	for s.Scan() {
		var (
			text  = s.Text()
			parts = strings.SplitN(text, ":", 3)
		)
		if len(parts) < 3 {
			return nil, errors.Errorf("invalid cgroup entry: %q", text)
		}
		for _, subs := range strings.Split(parts[1], ",") {
			if subs != "" {
				cgroups[subs] = parts[2]
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, errors.Errorf("buffer scanner failed: %v", err)
	}

	return cgroups, nil
}

// parseCgroupFile will read and verify the cgroup file entry of a container
func parseCgroupFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.Errorf("unable to parse cgroup file: %v", err)
	}
	defer file.Close()
	return parseCgroupFromReader(file)
}

// pidPath will get the pid path of the container
func pidPath(pid int) cgroups.Path {
	processPath := "/proc/" + strconv.Itoa(pid) + "/cgroup"
	paths, err := parseCgroupFile(processPath)
	if err != nil {
		return getErrorPath(errors.Wrapf(err, "parse cgroup file %s", processPath))
	}
	return getExistingPath(paths, pid, "")
}

// getCgroupDestination will validate the subsystem with the mountpath in container mountinfo file.
func getCgroupDestination(pid int, subsystem string) (string, error) {
	mountinfoPath := fmt.Sprintf("/proc/%d/mountinfo", pid)
	file, err := os.Open(mountinfoPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	s := bufio.NewScanner(file)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		for _, opt := range strings.Split(fields[len(fields)-1], ",") {
			if opt == subsystem {
				return fields[3], nil
			}
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", errors.Errorf("no destination found for %v ", subsystem)
}

// getExistingPath will be used to get the existing valid cgroup path
func getExistingPath(paths map[string]string, pid int, suffix string) cgroups.Path {
	for n, p := range paths {
		dest, err := getCgroupDestination(pid, n)
		if err != nil {
			return getErrorPath(err)
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return getErrorPath(err)
		}
		if rel == "." {
			rel = dest
		}
		paths[n] = filepath.Join("/", rel)
	}
	return func(name cgroups.Name) (string, error) {
		root, ok := paths[string(name)]
		if !ok {
			if root, ok = paths[fmt.Sprintf("name=%s", name)]; !ok {
				return "", cgroups.ErrControllerNotActive
			}
		}
		if suffix != "" {
			return filepath.Join(root, suffix), nil
		}
		return root, nil
	}
}

// getErrorPath will give the invalid cgroup path
func getErrorPath(err error) cgroups.Path {
	return func(_ cgroups.Name) (string, error) {
		return "", err
	}
}

// getCGroupManager will return the cgroup for the given pid of the process
func getCGroupManager(pid int, containerID string) (interface{}, error, string) {
	if cgroups.Mode() == cgroups.Unified {
		//groupPath, err := cgroupsv2.PidGroupPath(pid)
		//if err != nil {
		//	return nil, errors.Errorf("Error in getting groupPath, %v", err)
		//}
		groupPath := ""
		output, err := exec.Command("bash", "-c", fmt.Sprintf("nsenter -t 1 -C -m -- cat /proc/%v/cgroup", pid)).CombinedOutput()
		if err != nil {
			return nil, errors.Errorf("Error in getting groupPath,%s", string(output)), ""
		}
		parts := strings.SplitN(string(output), ":", 3)
		if len(parts) < 3 {
			return "", fmt.Errorf("invalid cgroup entry: %s", string(output)), ""
		}
		if parts[0] == "0" && parts[1] == "" {
			groupPath = parts[2]
		}
		log.Infof("group path: %s", groupPath)

		cgroup2, err := cgroupsv2.LoadManager("/sys/fs/cgroup", string(groupPath))
		if err != nil {
			return nil, errors.Errorf("Error loading cgroup v2 manager, %v", err), ""
		}
		return cgroup2, nil, groupPath
	}
	path := pidPath(pid)
	cgroup, err := findValidCgroup(path, containerID)
	if err != nil {
		return nil, errors.Errorf("fail to get cgroup, err: %v", err), ""
	}
	cgroup1, err := cgroups.Load(cgroups.V1, cgroups.StaticPath(cgroup))
	if err != nil {
		return nil, errors.Errorf("fail to load the cgroup, err: %v", err), ""
	}

	return cgroup1, nil, ""
}

// addProcessToCgroup will add the process to cgroup
// By default it will add to v1 cgroup
func addProcessToCgroup(pid int, control interface{}, groupPath string) error {
	if cgroups.Mode() == cgroups.Unified {
		//var cgroup1 = control.(*cgroupsv2.Manager)
		//return cgroup1.AddProc(uint64(pid))
		args := []string{"-t", "1", "-C", "--", "sudo", "sh", "-c",
			fmt.Sprintf("echo %d >> /sys/fs/cgroup%s/cgroup.procs", pid, strings.ReplaceAll(groupPath, "\n", ""))}
		output, err := exec.Command("nsenter", args...).CombinedOutput()
		if err != nil {
			logrus.Error(string(output))
			return err
		}
		return nil
	}
	var cgroup1 = control.(cgroups.Cgroup)
	return cgroup1.Add(cgroups.Process{Pid: pid})
}

// list of cgroups in a container
var (
	cgroupSubsystemList = []string{"cpu", "memory", "systemd", "net_cls",
		"net_prio", "freezer", "blkio", "perf_event", "devices", "cpuset",
		"cpuacct", "pids", "hugetlb",
	}
)

// findValidCgroup will be used to get a valid cgroup path
func findValidCgroup(path cgroups.Path, target string) (string, error) {
	for _, subsystem := range cgroupSubsystemList {
		path, err := path(cgroups.Name(subsystem))
		if err != nil {
			log.Errorf("fail to retrieve the cgroup path, subsystem: %v, target: %v, err: %v", subsystem, target, err)
			continue
		}
		if strings.Contains(path, target) {
			return path, nil
		}
	}
	return "", errors.Errorf("never found valid cgroup for %s", target)
}

// getENV fetches all the env variables from the runner pod
func getENV(experimentDetails *experimentTypes.ExperimentDetails) {
	experimentDetails.ExperimentName = types.Getenv("EXPERIMENT_NAME", "")
	experimentDetails.InstanceID = types.Getenv("INSTANCE_ID", "")
	experimentDetails.AppNS = types.Getenv("APP_NAMESPACE", "")
	experimentDetails.TargetContainer = types.Getenv("APP_CONTAINER", "")
	experimentDetails.TargetPods = types.Getenv("APP_POD", "")
	experimentDetails.ChaosDuration, _ = strconv.Atoi(types.Getenv("TOTAL_CHAOS_DURATION", "30"))
	experimentDetails.ChaosNamespace = types.Getenv("CHAOS_NAMESPACE", "litmus")
	experimentDetails.EngineName = types.Getenv("CHAOSENGINE", "")
	experimentDetails.ChaosUID = clientTypes.UID(types.Getenv("CHAOS_UID", ""))
	experimentDetails.ChaosPodName = types.Getenv("POD_NAME", "")
	experimentDetails.FillPercentage = types.Getenv("FILL_PERCENTAGE", "")
	experimentDetails.EphemeralStorageMebibytes = types.Getenv("EPHEMERAL_STORAGE_MEBIBYTES", "")
	experimentDetails.DataBlockSize, _ = strconv.Atoi(types.Getenv("DATA_BLOCK_SIZE", "256"))
	experimentDetails.FillPath = types.Getenv("FILL_PATH", "")
	experimentDetails.FileSize = types.Getenv("FILE_SIZE", "1G")
	experimentDetails.ContainerRuntime = types.Getenv("CONTAINER_RUNTIME", "docker")
	experimentDetails.SocketPath = types.Getenv("SOCKET_PATH", "/var/run/docker.sock")
}
