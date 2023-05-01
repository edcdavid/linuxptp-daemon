package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/linuxptp-daemon/pkg/config"
	"github.com/openshift/linuxptp-daemon/pkg/daemon"
	ptpv1 "github.com/openshift/ptp-operator/api/v1"
	ptpclient "github.com/openshift/ptp-operator/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type cliParams struct {
	updateInterval int
	profileDir     string
}

// Parse Command line flags
func flagInit(cp *cliParams) {
	flag.IntVar(&cp.updateInterval, "update-interval", config.DefaultUpdateInterval,
		"Interval to update PTP status")
	flag.StringVar(&cp.profileDir, "linuxptp-profile-path", config.DefaultProfilePath,
		"profile to start linuxptp processes")
}

func main() {
	cp := &cliParams{}
	flag.Parse()
	flagInit(cp)

	glog.Infof("resync period set to: %d [s]", cp.updateInterval)
	glog.Infof("linuxptp profile path set to: %s", cp.profileDir)

	cfg, err := config.GetKubeConfig()
	if err != nil {
		glog.Errorf("get kubeconfig failed: %v", err)
		return
	}
	glog.Infof("successfully get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Errorf("cannot create new config for kubeClient: %v", err)
		return
	}

	ptpClient, err := ptpclient.NewForConfig(cfg)
	if err != nil {
		glog.Errorf("cannot create new config for ptpClient: %v", err)
		return
	}

	// The name of NodePtpDevice CR for this node is equal to the node name
	nodeName := os.Getenv("NODE_NAME")
	podName := os.Getenv("POD_NAME")
	if nodeName == "" {
		glog.Error("cannot find NODE_NAME environment variable")
		return
	}

	// The name of NodePtpDevice CR for this node is equal to the node name
	var stdoutToSocket = false
	if val, ok := os.LookupEnv("LOGS_TO_SOCKET"); ok && val != "" {
		if ret, err := strconv.ParseBool(val); err == nil {
			stdoutToSocket = ret
		}
	}

	plugins := make([]string, 0)

	if val, ok := os.LookupEnv("PLUGINS"); ok && val != "" {
		plugins = strings.Split(val, ",")
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	ptpConfUpdate, err := daemon.NewLinuxPTPConfUpdate()
	if err != nil {
		glog.Errorf("failed to create a ptp config update: %v", err)
		return
	}

	// label the current linux-ptp-daemon pod with a nodeName label
	err = labelPod(kubeClient, nodeName, podName)
	if err != nil {
		glog.Errorf("failed to label linuxptp-daemon with node name, err: %v", err)
		return
	}

	hwconfigs := []ptpv1.HwConfig{}

	go daemon.New(
		nodeName,
		daemon.PtpNamespace,
		stdoutToSocket,
		kubeClient,
		ptpConfUpdate,
		stopCh,
		plugins,
		&hwconfigs,
	).Run()

	tickerPull := time.NewTicker(time.Second * time.Duration(cp.updateInterval))
	defer tickerPull.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// by default metrics is hosted here,if LOGS_TO_SOCKET variable is set then metrics are disabled
	if !stdoutToSocket { // if not sending metrics (log) out to a socket then host metrics here
		daemon.StartMetricsServer("0.0.0.0:9091")
	}

	for {
		select {
		case <-tickerPull.C:
			glog.Infof("ticker pull")
			// Run a loop to update the device status
			go daemon.RunDeviceStatusUpdate(ptpClient, nodeName, &hwconfigs)
			nodeProfile := filepath.Join(cp.profileDir, nodeName)
			if _, err := os.Stat(nodeProfile); err != nil {
				if os.IsNotExist(err) {
					glog.Infof("ptp profile doesn't exist for node: %v", nodeName)
					continue
				} else {
					glog.Errorf("error stating node profile %v: %v", nodeName, err)
					continue
				}
			}
			nodeProfilesJson, err := os.ReadFile(nodeProfile)
			if err != nil {
				glog.Errorf("error reading node profile: %v", nodeProfile)
				continue
			}

			err = ptpConfUpdate.UpdateConfig(nodeProfilesJson)
			if err != nil {
				glog.Errorf("error updating the node configuration using the profiles loaded: %v", err)
			}
		case sig := <-sigCh:
			glog.Info("signal received, shutting down", sig)
			return
		}
	}
}

type patchStringValue struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

func labelPod(kubeClient *kubernetes.Clientset, nodeName, podName string) (err error) {
	pod, err := kubeClient.CoreV1().Pods(daemon.PtpNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting linuxptp-daemon pod, err=%s", err)
	}
	if pod == nil {
		return fmt.Errorf("could not find linux-ptp-daemon pod to label")
	}
	if nodeName != "" && strings.Contains(nodeName, ".") {
		nodeName = strings.Split(nodeName, ".")[0]
	}

	payload := []patchStringValue{{
		Op:    "replace",
		Path:  "/metadata/labels/nodeName",
		Value: nodeName,
	}}
	payloadBytes, _ := json.Marshal(payload)

	_, err = kubeClient.CoreV1().Pods(pod.GetNamespace()).Patch(context.TODO(), pod.GetName(), types.JSONPatchType, payloadBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("could not label ns=%s pod %s, err=%s", pod.GetName(), pod.GetNamespace(), err)
	}
	glog.Infof("Pod %s labelled successfully.", pod.GetName())
	return nil
}
