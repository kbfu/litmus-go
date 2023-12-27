package helper

import (
	"context"
	"github.com/litmuschaos/litmus-go/pkg/clients"
	experimentTypes "github.com/litmuschaos/litmus-go/pkg/generic/godzilla-autoscaler/types"
	"github.com/litmuschaos/litmus-go/pkg/log"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var abort chan os.Signal

// Helper injects the network chaos
func Helper(clients clients.ClientSets) { // abort channel is used to transmit signal notifications.
	abort = make(chan os.Signal, 1)
	// Catch and relay certain signal(s) to abort channel.
	signal.Notify(abort, os.Interrupt, syscall.SIGTERM)
	experimentDetails := experimentTypes.ExperimentDetails{}
	getENV(&experimentDetails)

	logrus.Info("start scale experiment")
	switch strings.TrimSpace(strings.ToLower(experimentDetails.Kind)) {
	case "deployment":
		err := scaleDeployment(experimentDetails, clients.KubeClient)
		if err != nil {
			logrus.Fatalf("run failed, err: %v", err)
		}
	case "statefulset":
		err := scaleStatefulset(experimentDetails, clients.KubeClient)
		if err != nil {
			logrus.Fatalf("run failed, err: %v", err)
		}
	}
}

func scaleDeployment(experimentDetails experimentTypes.ExperimentDetails, client *kubernetes.Clientset) error {
	var originalNumber int32 = -1
	after := time.After(time.Duration(experimentDetails.Duration) * time.Second)
out:
	for {
		select {
		case <-after:
			logrus.Info("starting recovery")
			scale, err := client.AppsV1().Deployments(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			// recover
			scale.Spec.Replicas = originalNumber
			// scale
			_, err = client.AppsV1().Deployments(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Info("recovery completed")
			break out
		default:
			// fetch scale
			scale, err := client.AppsV1().Deployments(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			if originalNumber == -1 {
				originalNumber = scale.Spec.Replicas
			}
			go abortWatcher(client, "deployment", experimentDetails.Namespace, experimentDetails.Name, originalNumber)
			scale.Spec.Replicas = experimentDetails.TargetNumber
			// scale
			_, err = client.AppsV1().Deployments(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Infof("scale to %d", experimentDetails.TargetNumber)
			// wait for interval
			time.Sleep(time.Duration(experimentDetails.Interval) * time.Second)
			scale, err = client.AppsV1().Deployments(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			// recover
			scale.Spec.Replicas = originalNumber
			// scale
			_, err = client.AppsV1().Deployments(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Infof("recover to %d", originalNumber)
			// wait for interval
			time.Sleep(time.Duration(experimentDetails.Interval) * time.Second)
		}
	}
	return nil
}

func scaleStatefulset(experimentDetails experimentTypes.ExperimentDetails, client *kubernetes.Clientset) error {
	var originalNumber int32 = -1
	after := time.After(time.Duration(experimentDetails.Duration) * time.Second)
out:
	for {
		select {
		case <-after:
			logrus.Info("starting recovery")
			scale, err := client.AppsV1().StatefulSets(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			// recover
			scale.Spec.Replicas = originalNumber
			// scale
			_, err = client.AppsV1().StatefulSets(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Info("recovery completed")
			break out
		default:
			// fetch scale
			scale, err := client.AppsV1().StatefulSets(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			if originalNumber == -1 {
				originalNumber = scale.Spec.Replicas
			}
			go abortWatcher(client, "statefulset", experimentDetails.Namespace, experimentDetails.Name, originalNumber)
			scale.Spec.Replicas = experimentDetails.TargetNumber
			// scale
			_, err = client.AppsV1().StatefulSets(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Infof("scale to %d", experimentDetails.TargetNumber)
			// wait for interval
			time.Sleep(time.Duration(experimentDetails.Interval) * time.Second)
			scale, err = client.AppsV1().StatefulSets(experimentDetails.Namespace).GetScale(context.Background(), experimentDetails.Name, v1.GetOptions{})
			if err != nil {
				return err
			}
			// recover
			scale.Spec.Replicas = originalNumber
			// scale
			_, err = client.AppsV1().StatefulSets(experimentDetails.Namespace).UpdateScale(context.Background(), experimentDetails.Name, scale, v1.UpdateOptions{})
			if err != nil {
				return err
			}
			logrus.Infof("recover to %d", originalNumber)
			// wait for interval
			time.Sleep(time.Duration(experimentDetails.Interval) * time.Second)
		}
	}
	return nil
}

func getENV(experimentDetails *experimentTypes.ExperimentDetails) {
	experimentDetails.Kind = os.Getenv("APP_KIND")
	experimentDetails.Name = os.Getenv("APP_NAME")
	experimentDetails.Namespace = os.Getenv("APP_NAMESPACE")
	experimentDetails.Duration, _ = strconv.Atoi(os.Getenv("TOTAL_CHAOS_DURATION"))
	experimentDetails.Interval, _ = strconv.Atoi(os.Getenv("CHAOS_INTERVAL"))
	targetNumber, _ := strconv.Atoi(os.Getenv("TARGET_NUMBER"))
	experimentDetails.TargetNumber = int32(targetNumber)
}

// abortWatcher continuously watch for the abort signals
func abortWatcher(client *kubernetes.Clientset, kubeKind, namespace, name string, originalNumber int32) {
	<-abort
	log.Info("Chaos Revert Started")
	switch strings.TrimSpace(strings.ToLower(kubeKind)) {
	case "deployment":
		scale, err := client.AppsV1().Deployments(namespace).GetScale(context.Background(), name, v1.GetOptions{})
		if err != nil {
			logrus.Fatalf("recovery failed, error: %v", err)
		}
		// recover
		scale.Spec.Replicas = originalNumber
		// scale
		_, err = client.AppsV1().Deployments(namespace).UpdateScale(context.Background(), name, scale, v1.UpdateOptions{})
		if err != nil {
			logrus.Fatalf("recovery failed, error: %v", err)
		}
	case "statefulset":
		scale, err := client.AppsV1().StatefulSets(namespace).GetScale(context.Background(), name, v1.GetOptions{})
		if err != nil {
			logrus.Fatalf("recovery failed, error: %v", err)
		}
		// recover
		scale.Spec.Replicas = originalNumber
		// scale
		_, err = client.AppsV1().StatefulSets(namespace).UpdateScale(context.Background(), name, scale, v1.UpdateOptions{})
		if err != nil {
			logrus.Fatalf("recovery failed, error: %v", err)
		}
	}
	log.Info("Chaos Revert Completed")
	os.Exit(1)
}
