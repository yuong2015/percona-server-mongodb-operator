package stub

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Percona-Lab/percona-server-mongodb-operator/internal/mongod"
	"github.com/Percona-Lab/percona-server-mongodb-operator/internal/util"

	"github.com/operator-framework/operator-sdk/pkg/k8sclient"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

var execCommandTimeout = 30 * time.Second

// printOutput outputs stdout/stderr log buffers from commands
func printOutputBuffer(cmd, pod string, r io.Reader, out io.Writer) error {
	logrus.SetOutput(out)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fmt.Fprintf(out, "%s (%s): %s\n", cmd, pod, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		logrus.Errorf("Error printing output from %s (%s): %v", cmd, pod, err)
		return err
	}
	return nil
}

// printCommandOutput handles printing the stderr and stdout output of a remote command
func printCommandOutput(cmd, pod string, stdOut, stdErr *bytes.Buffer, out io.Writer) error {
	logrus.SetOutput(out)
	logrus.Infof("%s stdout:", cmd)
	err := printOutputBuffer(cmd, pod, stdOut, out)
	if err != nil {
		return err
	}
	if stdErr.Len() > 0 {
		logrus.Errorf("%s stderr:", cmd)
		err = printOutputBuffer(cmd, pod, stdErr, out)
		if err != nil {
			return err
		}
	}
	return nil
}

// execCommandInContainer runs a shell command inside a running container. This code is
// stolen from https://github.com/saada/mongodb-operator/blob/master/pkg/stub/handler.go.
// v2 of the core api should have features for doing this, move to using that later
//
// See: https://github.com/kubernetes/client-go/issues/45
//
func execCommandInContainer(pod corev1.Pod, containerName string, cmd []string) error {
	cfg := k8sclient.GetKubeConfig()
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %v", err)
	}

	// find the mongod container
	container := util.GetPodContainer(&pod, containerName)
	if container == nil {
		return nil
	}

	// find the mongod port
	containerPort := mongod.GetMongodPort(container)
	if containerPort == "" {
		return fmt.Errorf("cannot find mongod port in container: %s", container.Name)
	}

	type Output struct {
		stdout bytes.Buffer
		stderr bytes.Buffer
	}
	outChan := make(chan Output)

	go func() {
		req := client.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod.Name).
			Namespace(pod.Namespace).
			SubResource("exec")
		req.VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
		if err != nil {
			logrus.Errorf("failed to run exec in pod %s: %v", pod.Name, err)
			return
		}

		logrus.WithFields(logrus.Fields{
			"pod":       pod.Name,
			"container": containerName,
			"command":   cmd[0],
		}).Info("running command in container")

		out := Output{}
		err = exec.Stream(remotecommand.StreamOptions{
			Stdout: &out.stdout,
			Stderr: &out.stderr,
		})
		if err != nil {
			logrus.Errorf("error running remote command %s: %v", cmd[0], err)
		}
		outChan <- out
	}()

	select {
	case <-time.After(execCommandTimeout):
		logrus.Errorf("timeout reach executing command on pod %s: %s", pod.Name, cmd[0])
		return fmt.Errorf("timeout executing command")
	case out := <-outChan:
		return printCommandOutput(cmd[0], pod.Name, &out.stdout, &out.stderr, os.Stdout)
	}
}
