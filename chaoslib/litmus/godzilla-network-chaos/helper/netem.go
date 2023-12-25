package helper

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clients "github.com/litmuschaos/litmus-go/pkg/clients"
	"github.com/litmuschaos/litmus-go/pkg/events"
	experimentTypes "github.com/litmuschaos/litmus-go/pkg/generic/network-chaos/types"
	"github.com/litmuschaos/litmus-go/pkg/log"
	"github.com/litmuschaos/litmus-go/pkg/result"
	"github.com/litmuschaos/litmus-go/pkg/types"
	"github.com/litmuschaos/litmus-go/pkg/utils/common"
	clientTypes "k8s.io/apimachinery/pkg/types"
)

const (
	qdiscNotFound    = "Cannot delete qdisc with handle of zero"
	qdiscNoFileFound = "RTNETLINK answers: No such file or directory"
)

var (
	err           error
	inject, abort chan os.Signal
)

var destIps, sPorts, dPorts []string

// Helper injects the network chaos
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

	//Fetching all the ENV passed for the helper pod
	log.Info("[PreReq]: Getting the ENV variables")
	getENV(&experimentsDetails)

	// Intialise the chaos attributes
	types.InitialiseChaosVariables(&chaosDetails)

	// Intialise Chaos Result Parameters
	types.SetResultAttributes(&resultDetails, chaosDetails)

	// Set the chaos result uid
	result.SetResultUID(&resultDetails, clients, &chaosDetails)

	err := preparePodNetworkChaos(&experimentsDetails, clients, &eventsDetails, &chaosDetails, &resultDetails)
	if err != nil {
		log.Fatalf("helper pod failed, err: %v", err)
	}

}

// preparePodNetworkChaos contains the prepration steps before chaos injection
func preparePodNetworkChaos(experimentsDetails *experimentTypes.ExperimentDetails, clients clients.ClientSets, eventsDetails *types.EventDetails, chaosDetails *types.ChaosDetails, resultDetails *types.ResultDetails) error {

	containerID, err := common.GetContainerID(experimentsDetails.AppNS, experimentsDetails.TargetPods, experimentsDetails.TargetContainer, clients)
	if err != nil {
		return err
	}
	// extract out the pid of the target container
	targetPID, err := common.GetPID(experimentsDetails.ContainerRuntime, containerID, experimentsDetails.SocketPath)
	if err != nil {
		return err
	}

	// record the event inside chaosengine
	if experimentsDetails.EngineName != "" {
		msg := "Injecting " + experimentsDetails.ExperimentName + " chaos on application pod"
		types.SetEngineEventAttributes(eventsDetails, types.ChaosInject, msg, "Normal", chaosDetails)
		events.GenerateEvents(eventsDetails, clients, chaosDetails, "ChaosEngine")
	}

	// watching for the abort signal and revert the chaos
	go abortWatcher(targetPID, experimentsDetails.NetworkInterface, resultDetails.Name, chaosDetails.ChaosNamespace, experimentsDetails.TargetPods)

	// injecting network chaos inside target container
	if err = injectChaos(experimentsDetails, targetPID); err != nil {
		return err
	}

	if err = result.AnnotateChaosResult(resultDetails.Name, chaosDetails.ChaosNamespace, "injected", "pod", experimentsDetails.TargetPods); err != nil {
		return err
	}

	log.Infof("[Chaos]: Waiting for %vs", experimentsDetails.ChaosDuration)

	common.WaitForDuration(experimentsDetails.ChaosDuration)

	log.Info("[Chaos]: Stopping the experiment")

	// cleaning the netem process after chaos injection
	if err = killnetem(targetPID, experimentsDetails.NetworkInterface); err != nil {
		return err
	}

	return result.AnnotateChaosResult(resultDetails.Name, chaosDetails.ChaosNamespace, "reverted", "pod", experimentsDetails.TargetPods)
}

// injectChaos inject the network chaos in target container
// it is using nsenter command to enter into network namespace of target container
// and execute the netem command inside it.
func injectChaos(experimentDetails *experimentTypes.ExperimentDetails, pid int) error {
	switch experimentDetails.StressType {
	case "network-delay":
		// source
		for _, sport := range sPorts {
			output, err := exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n", "--", "sh", "-c",
				fmt.Sprintf("tcset %s --delay %d --src-port %s --delay-distro %d --add",
					experimentDetails.NetworkInterface, experimentDetails.NetworkLatency, sport, experimentDetails.Jitter)).CombinedOutput()
			if err != nil {
				logrus.Error(string(output))
				return err
			}
		}
		// destination
		var cmds []string
		baseCmd := fmt.Sprintf("tcset %s --delay %d --delay-distro %d --add",
			experimentDetails.NetworkInterface, experimentDetails.NetworkLatency, experimentDetails.Jitter)
		for _, dip := range destIps {
			tcsetCmd := baseCmd + fmt.Sprintf(" --network %s", dip)
			for _, dport := range dPorts {
				tcsetCmd += fmt.Sprintf(" --port %s", dport)
				cmds = append(cmds, tcsetCmd)
			}
		}
		if len(destIps) == 0 {
			for _, dport := range dPorts {
				tcsetCmd := baseCmd + fmt.Sprintf(" --port %s", dport)
				cmds = append(cmds, tcsetCmd)
			}
		}
		// show some message
		if len(cmds) == 0 {
			logrus.Infof("nothing will be executed")
		}
		for _, cmd := range cmds {
			logrus.Infof("executing command nsenter -t %d -n -- sh -c %s", pid, cmd)
			output, err := exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n", "--", "sh", "-c",
				cmd).CombinedOutput()
			if err != nil {
				logrus.Error(string(output))
				return err
			}
		}

	}
	return nil
}

// killnetem kill the netem process for all the target containers
func killnetem(PID int, networkInterface string) error {
	output, err := exec.Command("nsenter", "-t", fmt.Sprintf("%d", PID), "-n", "--", "sh", "-c",
		fmt.Sprintf("tcdel %s -a", networkInterface)).CombinedOutput()
	log.Info(string(output))

	if err != nil {
		// ignoring err if qdisc process doesn't exist inside the target container
		if strings.Contains(string(output), qdiscNotFound) || strings.Contains(string(output), qdiscNoFileFound) {
			log.Warn("The network chaos process has already been removed")
			return nil
		}
		return err
	}

	return nil
}

// getENV fetches all the env variables from the runner pod
func getENV(experimentDetails *experimentTypes.ExperimentDetails) {
	experimentDetails.ExperimentName = types.Getenv("EXPERIMENT_NAME", "")
	experimentDetails.InstanceID = types.Getenv("INSTANCE_ID", "")
	experimentDetails.AppNS = types.Getenv("APP_NAMESPACE", "")
	experimentDetails.TargetContainer = types.Getenv("APP_CONTAINER", "")
	experimentDetails.TargetPods = types.Getenv("APP_POD", "")
	experimentDetails.AppLabel = types.Getenv("APP_LABEL", "")
	experimentDetails.ChaosDuration, _ = strconv.Atoi(types.Getenv("TOTAL_CHAOS_DURATION", "30"))
	experimentDetails.ChaosNamespace = types.Getenv("CHAOS_NAMESPACE", "litmus")
	experimentDetails.EngineName = types.Getenv("CHAOSENGINE", "")
	experimentDetails.ChaosUID = clientTypes.UID(types.Getenv("CHAOS_UID", ""))
	experimentDetails.ChaosPodName = types.Getenv("POD_NAME", "")
	experimentDetails.ContainerRuntime = types.Getenv("CONTAINER_RUNTIME", "")
	experimentDetails.NetworkInterface = types.Getenv("NETWORK_INTERFACE", "eth0")
	experimentDetails.SocketPath = types.Getenv("SOCKET_PATH", "")
	experimentDetails.DestinationIPs = types.Getenv("DESTINATION_IPS", "")
	experimentDetails.SourcePorts = types.Getenv("SOURCE_PORTS", "")
	experimentDetails.DestinationPorts = types.Getenv("DESTINATION_PORTS", "")
	experimentDetails.StressType = types.Getenv("STRESS_TYPE", "")
	experimentDetails.Jitter, _ = strconv.Atoi(types.Getenv("JITTER", ""))
	experimentDetails.NetworkLatency, _ = strconv.Atoi(types.Getenv("NETWORK_LATENCY", ""))

	destIps = getDestinationIPs(experimentDetails.DestinationIPs)
	if strings.TrimSpace(experimentDetails.DestinationPorts) != "" {
		dPorts = strings.Split(strings.TrimSpace(experimentDetails.DestinationPorts), ",")
	}
	if strings.TrimSpace(experimentDetails.SourcePorts) != "" {
		sPorts = strings.Split(strings.TrimSpace(experimentDetails.SourcePorts), ",")
	}
}

func getDestinationIPs(ips string) []string {
	if strings.TrimSpace(ips) == "" {
		return nil
	}
	destIPs := strings.Split(strings.TrimSpace(ips), ",")
	var uniqueIps []string

	// removing duplicates ips from the list, if any
	for i := range destIPs {
		if !common.Contains(destIPs[i], uniqueIps) {
			uniqueIps = append(uniqueIps, destIPs[i])
		}
	}
	return uniqueIps
}

// abortWatcher continuously watch for the abort signals
func abortWatcher(targetPID int, networkInterface, resultName, chaosNS, targetPodName string) {

	<-abort
	log.Info("[Chaos]: Killing process started because of terminated signal received")
	log.Info("Chaos Revert Started")
	// retry thrice for the chaos revert
	retry := 3
	for retry > 0 {
		if err = killnetem(targetPID, networkInterface); err != nil {
			log.Errorf("unable to kill netem process, err :%v", err)
		}
		if err == nil {
			break
		}
		retry--
		time.Sleep(1 * time.Second)
	}
	if err = result.AnnotateChaosResult(resultName, chaosNS, "reverted", "pod", targetPodName); err != nil {
		log.Errorf("unable to annotate the chaosresult, err :%v", err)
	}
	log.Info("Chaos Revert Completed")
	os.Exit(1)
}
