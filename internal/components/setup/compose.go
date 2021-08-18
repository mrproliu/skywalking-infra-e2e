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
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/apache/skywalking-infra-e2e/internal/config"
	"github.com/apache/skywalking-infra-e2e/internal/constant"
	"github.com/apache/skywalking-infra-e2e/internal/logger"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"github.com/testcontainers/testcontainers-go"
)

// ComposeSetup sets up environment according to e2e.yaml.
func ComposeSetup(e2eConfig *config.E2EConfig) error {
	composeConfigPath := e2eConfig.Setup.GetFile()
	if composeConfigPath == "" {
		return fmt.Errorf("no compose config file was provided")
	}

	// build docker client
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}

	logger.Log.Infof("[print]current docker daemon host: %s", cli.DaemonHost())
	logger.Log.Infof("[print]in a container: %b", inAContainer())
	network, err := getDefaultNetwork(context.Background(), *cli)
	logger.Log.Infof("[print]docker default network name: %s", network)
	ip, err := getGatewayIP(context.Background(), *cli)
	logger.Log.Infof("[print]gateway ip: %s", ip)

	// setup docker compose
	composeFilePaths := []string{
		composeConfigPath,
	}
	identifier := GetIdentity()
	compose := testcontainers.NewLocalDockerCompose(composeFilePaths, identifier)

	// bind wait port
	serviceWithPorts, err := bindWaitPort(e2eConfig, compose)
	if err != nil {
		return fmt.Errorf("bind wait ports error: %v", err)
	}

	execError := compose.WithCommand([]string{"up", "-d"}).Invoke()
	if execError.Error != nil {
		return execError.Error
	}

	// find exported port and build env
	for service, portList := range serviceWithPorts {
		container, err2 := findContainer(cli, fmt.Sprintf("%s_%s", identifier, getInstanceName(service)))
		if err2 != nil {
			return err2
		}
		if len(portList) == 0 {
			continue
		}

		containerPorts := container.Ports

		// get real ip address for access and export to env
		host := ip
		// format: <service_name>_host
		if err2 := exportComposeEnv(fmt.Sprintf("%s_host", service), host, service); err2 != nil {
			return err2
		}

		ports, _ := Ports(context.Background(), cli, container)
		for port := range ports {
			logger.Log.Infof("[print]ports list to %s, protocol: %s, port: %d, count of bind: %d",
				service, port.Proto(), port.Int(), len(ports[port]))
			if len(ports[port]) > 0 {
				for _, p := range ports[port] {
					logger.Log.Infof("[print] ---host: %s, port: %s", p.HostIP, p.HostPort)
				}
			}
		}

		for inx := range portList {
			for _, containerPort := range containerPorts {
				if int(containerPort.PrivatePort) != portList[inx].expectPort {
					continue
				}

				realExpectPort, netmode, err := MappedPort(context.Background(), cli, container, nat.Port(fmt.Sprintf("%d/tcp", portList[inx].expectPort)))
				logger.Log.Infof("[print]find mapped service: %s, expectPort: %d, protocol: %s, port: %s, netmode: %s, error: %v",
					service, portList[inx].expectPort, realExpectPort.Proto(), realExpectPort.Port(), netmode, err)

				// external check
				dialer := net.Dialer{}
				address := net.JoinHostPort(ip, fmt.Sprintf("%d", containerPort.PublicPort))
				for {
					logger.Log.Infof("[print]trying to connect to %s", address)
					conn, err := dialer.DialContext(context.Background(), "tcp", address)
					if err != nil {
						logger.Log.Errorf("[print]connect error: %v", err)
						time.Sleep(time.Second * 2)
					} else {
						conn.Close()
						logger.Log.Infof("[print]connect success to %s", address)
						break
					}
				}

				// internal check
				command := buildInternalCheckCommand(int(containerPort.PrivatePort))
				for {
					exitCode, err := Exec(context.Background(), *cli, container, []string{"/bin/sh", "-c", command})
					if err != nil {
						return fmt.Errorf("host port waiting failed: %v", err)
					}

					if exitCode == 0 {
						break
					} else if exitCode == 126 {
						return fmt.Errorf("/bin/sh command not executable")
					}
				}
				logger.Log.Infof("[print]connect success to internal port: %d", containerPort.PrivatePort)

				// expose env config to env
				// format: <service_name>_<port>
				if err2 := exportComposeEnv(
					fmt.Sprintf("%s_%d", service, containerPort.PrivatePort),
					fmt.Sprintf("%d", containerPort.PublicPort),
					service); err2 != nil {
					return err2
				}
				break
			}
		}
	}

	// run steps
	err = RunStepsAndWait(e2eConfig.Setup.Steps, e2eConfig.Setup.Timeout, nil)
	if err != nil {
		logger.Log.Errorf("execute steps error: %v", err)
		return err
	}

	return nil
}

func exportComposeEnv(key, value, service string) error {
	err := os.Setenv(key, value)
	if err != nil {
		return fmt.Errorf("could not set env for %s, %v", service, err)
	}
	logger.Log.Infof("expose env : %s : %s", key, value)
	return nil
}

func bindWaitPort(e2eConfig *config.E2EConfig, compose *testcontainers.LocalDockerCompose) (map[string][]*hostPortCachedStrategy, error) {
	timeout := e2eConfig.Setup.Timeout
	var waitTimeout time.Duration
	if timeout <= 0 {
		waitTimeout = constant.DefaultWaitTimeout
	} else {
		waitTimeout = time.Duration(timeout) * time.Second
	}
	serviceWithPorts := make(map[string][]*hostPortCachedStrategy)
	for service, content := range compose.Services {
		serviceConfig := content.(map[interface{}]interface{})
		ports := serviceConfig["ports"]
		if ports == nil {
			continue
		}
		serviceWithPorts[service] = []*hostPortCachedStrategy{}

		portList := ports.([]interface{})
		for inx := range portList {
			exportPort, err := getExpectPort(portList[inx])
			if err != nil {
				return nil, err
			}

			strategy := &hostPortCachedStrategy{
				expectPort:       exportPort,
				HostPortStrategy: *wait.NewHostPortStrategy(nat.Port(fmt.Sprintf("%d/tcp", exportPort))).WithStartupTimeout(waitTimeout),
			}
			//compose.WithExposedService(service, exportPort, strategy)

			serviceWithPorts[service] = append(serviceWithPorts[service], strategy)
		}
	}
	return serviceWithPorts, nil
}

func getExpectPort(portConfig interface{}) (int, error) {
	switch conf := portConfig.(type) {
	case int:
		return conf, nil
	case string:
		portInfo := strings.Split(conf, ":")
		if len(portInfo) > 1 {
			return strconv.Atoi(portInfo[1])
		}
		return strconv.Atoi(portInfo[0])
	}
	return 0, fmt.Errorf("unknown port information: %v", portConfig)
}

func findContainer(c *client.Client, instanceName string) (*types.Container, error) {
	f := filters.NewArgs(filters.Arg("name", instanceName))
	containerListOptions := types.ContainerListOptions{Filters: f}
	containers, err := c.ContainerList(context.Background(), containerListOptions)
	if err != nil {
		return nil, err
	}

	if len(containers) == 0 {
		return nil, fmt.Errorf("could not found container: %s", instanceName)
	}
	return &containers[0], nil
}

func getInstanceName(serviceName string) string {
	match, err := regexp.MatchString(".*_[0-9]+", serviceName)
	if err != nil {
		return serviceName
	}
	if !match {
		return serviceName + "_1"
	}
	return serviceName
}

// hostPortCachedStrategy cached original target
type hostPortCachedStrategy struct {
	wait.HostPortStrategy
	expectPort int
	target     wait.StrategyTarget
}

func (hp *hostPortCachedStrategy) WaitUntilReady(ctx context.Context, target wait.StrategyTarget) error {
	hp.target = target
	return hp.HostPortStrategy.WaitUntilReady(ctx, target)
}

func inAContainer() bool {
	// see https://github.com/testcontainers/testcontainers-java/blob/3ad8d80e2484864e554744a4800a81f6b7982168/core/src/main/java/org/testcontainers/dockerclient/DockerClientConfigUtils.java#L15
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func getGatewayIP(ctx context.Context, cli client.Client) (string, error) {
	// Use a default network as defined in the DockerProvider
	network, err := getDefaultNetwork(ctx, cli)
	nw, err := GetNetwork(ctx, cli, network)
	if err != nil {
		return "", err
	}

	var ip string
	for _, config := range nw.IPAM.Config {
		if config.Gateway != "" {
			ip = config.Gateway
			break
		}
	}
	if ip == "" {
		return "", fmt.Errorf("Failed to get gateway IP from network settings")
	}

	return ip, nil
}

func GetNetwork(ctx context.Context, cli client.Client, name string) (types.NetworkResource, error) {
	networkResource, err := cli.NetworkInspect(ctx, name, types.NetworkInspectOptions{
		Verbose: true,
	})
	if err != nil {
		return types.NetworkResource{}, err
	}

	return networkResource, err
}

func getDefaultNetwork(ctx context.Context, cli client.Client) (string, error) {
	// Get list of available networks
	networkResources, err := cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return "", err
	}

	reaperNetwork := testcontainers.ReaperDefault

	reaperNetworkExists := false

	for _, net := range networkResources {
		if net.Name == "bridge" {
			return "bridge", nil
		}

		if net.Name == reaperNetwork {
			reaperNetworkExists = true
		}
	}

	// Create a bridge network for the container communications
	if !reaperNetworkExists {
		_, err = cli.NetworkCreate(ctx, reaperNetwork, types.NetworkCreate{
			Driver:     "bridge",
			Attachable: true,
			Labels: map[string]string{
				"org.testcontainers.golang": "true",
			},
		})

		if err != nil {
			return "", err
		}
	}

	return reaperNetwork, nil
}

func buildInternalCheckCommand(internalPort int) string {
	command := `(
					cat /proc/net/tcp* | awk '{print $2}' | grep -i :%04x ||
					nc -vz -w 1 localhost %d ||
					/bin/sh -c '</dev/tcp/localhost/%d'
				)
				`
	return "true && " + fmt.Sprintf(command, internalPort, internalPort, internalPort)
}

func Exec(ctx context.Context, cli client.Client, c *types.Container, cmd []string) (int, error) {
	response, err := cli.ContainerExecCreate(ctx, c.ID, types.ExecConfig{
		Cmd:    cmd,
		Detach: false,
	})
	if err != nil {
		return 0, err
	}

	err = cli.ContainerExecStart(ctx, response.ID, types.ExecStartCheck{
		Detach: false,
	})
	if err != nil {
		return 0, err
	}

	var exitCode int
	for {
		execResp, err := cli.ContainerExecInspect(ctx, response.ID)
		if err != nil {
			return 0, err
		}

		if !execResp.Running {
			exitCode = execResp.ExitCode
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	return exitCode, nil
}

func MappedPort(ctx context.Context, cli *client.Client, container *types.Container, port nat.Port) (nat.Port, container.NetworkMode, error) {
	inspect, err := inspectContainer(ctx, cli, container)
	if err != nil {
		return "", "", err
	}
	if inspect.ContainerJSONBase.HostConfig.NetworkMode == "host" {
		return port, inspect.ContainerJSONBase.HostConfig.NetworkMode, nil
	}
	ports, err := Ports(ctx, cli, container)
	if err != nil {
		return "", inspect.ContainerJSONBase.HostConfig.NetworkMode, err
	}

	for k, p := range ports {
		if k.Port() != port.Port() {
			continue
		}
		if port.Proto() != "" && k.Proto() != port.Proto() {
			continue
		}
		if len(p) == 0 {
			continue
		}
		newPort, err := nat.NewPort(k.Proto(), p[0].HostPort)
		return newPort, inspect.ContainerJSONBase.HostConfig.NetworkMode, err
	}

	return "", inspect.ContainerJSONBase.HostConfig.NetworkMode, fmt.Errorf("port not found")
}

func Ports(ctx context.Context, cli *client.Client, container *types.Container) (nat.PortMap, error) {
	inspect, err := inspectContainer(ctx, cli, container)
	if err != nil {
		return nil, err
	}
	return inspect.NetworkSettings.Ports, nil
}

func inspectContainer(ctx context.Context, cli *client.Client, container *types.Container) (*types.ContainerJSON, error) {
	inspect, err := cli.ContainerInspect(ctx, container.ID)
	if err != nil {
		return nil, err
	}

	return &inspect, nil
}
