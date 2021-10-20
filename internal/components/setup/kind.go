// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.
//

package setup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	apiv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	ctlwait "k8s.io/kubectl/pkg/cmd/wait"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	ctlutil "k8s.io/kubectl/pkg/util"

	kind "sigs.k8s.io/kind/cmd/kind/app"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"

	"github.com/apache/skywalking-infra-e2e/internal/config"
	"github.com/apache/skywalking-infra-e2e/internal/constant"
	"github.com/apache/skywalking-infra-e2e/internal/logger"
	"github.com/apache/skywalking-infra-e2e/internal/util"
)

var (
	kindConfigPath string
	kubeConfigPath string

	portForwardContext *kindPortForwardContext
)

type kindPortForwardContext struct {
	ctx                     context.Context
	cancelFunc              context.CancelFunc
	stopChannel             chan struct{}
	resourceCount           int
	resourceFinishedChannel chan struct{}
}

type kindPort struct {
	inputPort  string // User input port
	realPort   int    // Real remote port, deference with input when resource is service or use port name
	waitExpose string // Need to use when expose
}

// KindSetup sets up environment according to e2e.yaml.
func KindSetup(e2eConfig *config.E2EConfig) error {
	kindConfigPath = e2eConfig.Setup.GetFile()

	if kindConfigPath == "" {
		return fmt.Errorf("no kind config file was provided")
	}

	steps := e2eConfig.Setup.Steps
	// if no steps was provided, then no need to create the cluster.
	if steps == nil {
		logger.Log.Info("no steps is provided")
		return nil
	}

	// export env file
	if e2eConfig.Setup.InitSystemEnvironment != "" {
		profilePath := util.ResolveAbs(e2eConfig.Setup.InitSystemEnvironment)
		util.ExportEnvVars(profilePath)
	}

	if err := createKindCluster(kindConfigPath); err != nil {
		return err
	}

	// import images
	if len(e2eConfig.Setup.Kind.ImportImages) > 0 {
		for _, image := range e2eConfig.Setup.Kind.ImportImages {
			image = os.ExpandEnv(image)
			args := []string{"load", "docker-image", image}

			logger.Log.Infof("import docker images: %s", image)
			if err := kind.Run(kindcmd.NewLogger(), kindcmd.StandardIOStreams(), args); err != nil {
				return err
			}
		}
	}

	cluster, err := util.ConnectToK8sCluster(kubeConfigPath)
	if err != nil {
		logger.Log.Errorf("connect to k8s cluster failed according to config file: %s", kubeConfigPath)
		return err
	}

	// run steps
	err = RunStepsAndWait(e2eConfig.Setup.Steps, e2eConfig.Setup.Timeout, cluster)
	if err != nil {
		logger.Log.Errorf("execute steps error: %v", err)
		return err
	}

	// expose ports
	err = exposeKindService(e2eConfig.Setup.Kind.ExposePorts, e2eConfig.Setup.Timeout, kubeConfigPath)
	if err != nil {
		logger.Log.Errorf("export ports error: %v", err)
		return err
	}
	return nil
}

func KindShouldWaitSignal() bool {
	return portForwardContext != nil && portForwardContext.resourceCount > 0
}

// KindCleanNotify notify when clean up
func KindCleanNotify() {
	if portForwardContext != nil {
		portForwardContext.stopChannel <- struct{}{}
		// wait all stopped
		for i := 0; i < portForwardContext.resourceCount; i++ {
			<-portForwardContext.resourceFinishedChannel
		}
	}
}

func createKindCluster(kindConfigPath string) error {
	// the config file name of the k8s cluster that kind create
	kubeConfigPath = constant.K8sClusterConfigFilePath
	args := []string{"create", "cluster", "--config", kindConfigPath, "--kubeconfig", kubeConfigPath}

	logger.Log.Info("creating kind cluster...")
	logger.Log.Debugf("cluster create commands: %s %s", constant.KindCommand, strings.Join(args, " "))
	if err := kind.Run(kindcmd.NewLogger(), kindcmd.StandardIOStreams(), args); err != nil {
		return err
	}
	logger.Log.Info("create kind cluster succeeded")

	// export kubeconfig path for command line
	err := os.Setenv("KUBECONFIG", kubeConfigPath)
	if err != nil {
		return fmt.Errorf("could not export kubeconfig file path, %v", err)
	}
	logger.Log.Infof("export KUBECONFIG=%s", kubeConfigPath)
	return nil
}

func getWaitOptions(kubeConfigYaml []byte, wait *config.Wait) (options *ctlwait.WaitOptions, err error) {
	if strings.Contains(wait.Resource, "/") && wait.LabelSelector != "" {
		return nil, fmt.Errorf("when passing resource.group/resource.name in Resource, the labelSelector can not be set at the same time")
	}

	restClientGetter := util.NewSimpleRESTClientGetter(wait.Namespace, string(kubeConfigYaml))
	silenceOutput, _ := os.Open(os.DevNull)
	ioStreams := genericclioptions.IOStreams{In: os.Stdin, Out: silenceOutput, ErrOut: os.Stderr}
	waitFlags := ctlwait.NewWaitFlags(restClientGetter, ioStreams)
	// global timeout is set in e2e.yaml
	waitFlags.Timeout = constant.SingleDefaultWaitTimeout
	waitFlags.ForCondition = wait.For

	var args []string
	// resource.group/resource.name OR resource.group
	if wait.Resource != "" {
		args = append(args, wait.Resource)
	} else {
		return nil, fmt.Errorf("resource must be provided in wait block")
	}

	if wait.LabelSelector != "" {
		waitFlags.ResourceBuilderFlags.LabelSelector = &wait.LabelSelector
	} else if !strings.Contains(wait.Resource, "/") {
		// if labelSelector is nil and resource only provide resource.group, check all resources.
		waitFlags.ResourceBuilderFlags.All = &constant.True
	}

	options, err = waitFlags.ToOptions(args)
	if err != nil {
		return nil, err
	}
	return options, nil
}

func createByManifest(c *kubernetes.Clientset, dc dynamic.Interface, manifest config.Manifest) error {
	files, err := util.GetManifests(manifest.Path)
	if err != nil {
		logger.Log.Error("get manifests failed")
		return err
	}

	for _, f := range files {
		logger.Log.Infof("creating manifest %s", f)
		err = util.OperateManifest(c, dc, f, apiv1.Create)
		if err != nil {
			logger.Log.Errorf("create manifest %s failed", f)
			return err
		}
	}
	return nil
}

func concurrentlyWait(wait *config.Wait, options *ctlwait.WaitOptions, waitSet *util.WaitSet) {
	defer waitSet.WaitGroup.Done()

	err := options.RunWait()
	if err != nil {
		err = fmt.Errorf("wait strategy :%+v, err: %s", wait, err)
		waitSet.ErrChan <- err
		return
	}
	logger.Log.Infof("wait %+v condition met", wait)
}

// buildKindPort for help find real pod remote port
func buildKindPort(port string, ro runtime.Object, pod *v1.Pod) (*kindPort, error) {
	var needExpose, remotePort string
	if strings.Contains(port, ":") {
		needExpose = port
		remotePort = strings.Split(port, ":")[1]
	} else {
		needExpose = fmt.Sprintf(":%s", port)
		remotePort = port
	}

	service, isService := ro.(*v1.Service)
	if !isService {
		remotePortInt, err := strconv.Atoi(remotePort)
		if err != nil {
			containerPort, err := ctlutil.LookupContainerPortNumberByName(*pod, remotePort)
			if err != nil {
				return nil, err
			}

			remotePortInt = int(containerPort)
		}
		return &kindPort{
			inputPort:  remotePort,
			realPort:   remotePortInt,
			waitExpose: needExpose,
		}, nil
	}

	portnum64, err := strconv.ParseInt(remotePort, 10, 32)
	var portnum int32
	if err != nil {
		svcPort, err1 := ctlutil.LookupServicePortNumberByName(*service, remotePort)
		if err1 != nil {
			return nil, err1
		}
		portnum = svcPort
	} else {
		portnum = int32(portnum64)
	}
	containerPort, err := ctlutil.LookupContainerPortNumberByServicePort(*service, *pod, portnum)
	if err != nil {
		// can't resolve a named port, or Service did not declare this port, return an error
		return nil, err
	}

	// convert the resolved target port back to a string
	realPort := int(containerPort)
	if strconv.Itoa(realPort) != remotePort {
		var localPort string
		if strings.Contains(port, ":") {
			localPort = strings.Split(port, ":")[0]
		}
		needExpose = fmt.Sprintf("%s:%d", localPort, realPort)
	}

	return &kindPort{
		inputPort:  remotePort,
		realPort:   realPort,
		waitExpose: needExpose,
	}, nil
}

func exposePerKindService(port config.KindExposePort, timeout time.Duration, clientGetter *util.SimpleRESTClientGetter,
	client *rest.RESTClient, roundTripper http.RoundTripper, upgrader spdy.Upgrader, forward *kindPortForwardContext) error {
	// find resource
	builder := resource.NewBuilder(clientGetter).
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		ContinueOnError().
		NamespaceParam(port.Namespace).DefaultNamespace()
	builder.ResourceNames("pods", port.Resource)
	obj, err := builder.Do().Object()
	if err != nil {
		return err
	}
	forwardablePod, err := polymorphichelpers.AttachablePodForObjectFn(clientGetter, obj, timeout)
	if err != nil {
		return err
	}

	// build port forward request
	req := client.Post().
		Resource("pods").
		Namespace(forwardablePod.Namespace).
		Name(forwardablePod.Name).
		SubResource("portforward")

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, req.URL())

	// build ports
	ports := strings.Split(port.Port, ",")
	convertedPorts := make([]*kindPort, len(ports))
	exposePorts := make([]string, len(ports))
	for i, p := range ports {
		if convertedPorts[i], err = buildKindPort(p, obj, forwardablePod); err != nil {
			return err
		}
		exposePorts[i] = convertedPorts[i].waitExpose
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readyChannel := make(chan struct{}, 1)
	forwardErrorChannel := make(chan error, 1)

	forwarder, err := portforward.New(dialer, exposePorts, forward.stopChannel, readyChannel,
		bufio.NewWriter(&stdout), bufio.NewWriter(&stderr))
	if err != nil {
		return err
	}

	// start forward
	go func() {
		if err = forwarder.ForwardPorts(); err != nil {
			forwardErrorChannel <- err
		}
		forward.resourceFinishedChannel <- struct{}{}
	}()

	// wait port forward result
	select {
	case <-readyChannel:
		exportedPorts, err1 := forwarder.GetPorts()
		if err1 != nil {
			return err1
		}

		// format: <resource>_host
		resourceName := port.Resource
		resourceName = strings.ReplaceAll(resourceName, "/", "_")
		resourceName = strings.ReplaceAll(resourceName, "-", "_")
		if err1 := exportKindEnv(fmt.Sprintf("%s_host", resourceName),
			"localhost", port.Resource); err1 != nil {
			return err1
		}

		// format: <resource>_<need_export_port>
		for _, p := range exportedPorts {
			for _, kp := range convertedPorts {
				if int(p.Remote) == kp.realPort {
					if err1 := exportKindEnv(fmt.Sprintf("%s_%s", resourceName, kp.inputPort),
						fmt.Sprintf("%d", p.Local), port.Resource); err1 != nil {
						return err1
					}
				}
			}
		}

	case err = <-forwardErrorChannel:
		return fmt.Errorf("create forward error, %s : %v", stderr.String(), err)
	}
	return nil
}

func exposeKindService(exports []config.KindExposePort, timeout int, kubeConfig string) error {
	// round tripper
	kubeConfigYaml, err := ioutil.ReadFile(kubeConfig)
	if err != nil {
		return err
	}
	clientGetter := util.NewSimpleRESTClientGetter("", string(kubeConfigYaml))

	restConf, err := clientGetter.ToRESTConfig()
	if err != nil {
		return err
	}
	restConf.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	tripperFor, upgrader, err := spdy.RoundTripperFor(restConf)
	if err != nil {
		return err
	}

	// rest client
	if restConf.GroupVersion == nil {
		restConf.GroupVersion = &schema.GroupVersion{Version: "v1"}
	}
	restConf.APIPath = "/api"
	client, err := rest.RESTClientFor(restConf)
	if err != nil {
		return err
	}

	// timeout
	var waitTimeout time.Duration
	if timeout <= 0 {
		waitTimeout = constant.DefaultWaitTimeout
	} else {
		waitTimeout = time.Duration(timeout) * time.Second
	}

	// stop port-forward channel
	childCtx, cancelFunc := context.WithCancel(context.Background())
	forwardContext := &kindPortForwardContext{
		ctx:                     childCtx,
		cancelFunc:              cancelFunc,
		stopChannel:             make(chan struct{}, 1),
		resourceFinishedChannel: make(chan struct{}, len(exports)),
		resourceCount:           len(exports),
	}
	for _, p := range exports {
		if err = exposePerKindService(p, waitTimeout, clientGetter, client, tripperFor, upgrader, forwardContext); err != nil {
			cancelFunc()
			return err
		}
	}

	// bind context
	portForwardContext = forwardContext
	return nil
}

func exportKindEnv(key, value, res string) error {
	err := os.Setenv(key, value)
	if err != nil {
		return fmt.Errorf("could not set env for %s, %v", res, err)
	}
	logger.Log.Infof("export %s=%s", key, value)
	return nil
}
