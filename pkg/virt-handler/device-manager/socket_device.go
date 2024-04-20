/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2023 Red Hat, Inc.
 *
 */

package device_manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/util"
	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
	"kubevirt.io/kubevirt/pkg/virt-handler/selinux"
)

type SocketDevicePlugin struct {
	DevicePluginBase
	socketDir  string
	socket     string
	socketName string
}

func NewSocketDevicePlugin(socketName, socketDir, socket string, maxDevices int) *SocketDevicePlugin {
	dpi := &SocketDevicePlugin{
		DevicePluginBase: DevicePluginBase{
			health:       make(chan deviceHealth),
			resourceName: fmt.Sprintf("%s/%s", DeviceNamespace, socketName),
			initialized:  false,
			lock:         &sync.Mutex{},
			done:         make(chan struct{}),
			deregistered: make(chan struct{}),
			socketPath:   SocketPath(strings.Replace(socketName, "/", "-", -1)),
		},
		socket:     socket,
		socketDir:  socketDir,
		socketName: socketName,
	}
	dpi.registerFunc = dpi.registerServer
	dpi.healthcheck = dpi.healthCheck
	for i := 0; i < maxDevices; i++ {
		deviceId := dpi.socketName + strconv.Itoa(i)
		dpi.devs = append(dpi.devs, &pluginapi.Device{
			ID:     deviceId,
			Health: pluginapi.Healthy,
		})
	}
	return dpi
}

func (dpi *SocketDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.DefaultLogger().Infof("Socket Allocate: resourceName: %s", dpi.socketName)
	log.DefaultLogger().Infof("Socket Allocate: request: %v", r.ContainerRequests)
	response := pluginapi.AllocateResponse{}
	containerResponse := new(pluginapi.ContainerAllocateResponse)

	prSock, err := safepath.JoinAndResolveWithRelativeRoot("/", dpi.socketDir, dpi.socket)
	if err != nil {
		return nil, fmt.Errorf("error opening the socket %s/%s: %v", dpi.socketDir, dpi.socket, err)
	}
	err = safepath.ChownAtNoFollow(prSock, util.NonRootUID, util.NonRootUID)
	if err != nil {
		return nil, fmt.Errorf("error setting the permission the socket %s/%s:%v", dpi.socketDir, dpi.socket, err)
	}
	if se, exists, err := selinux.NewSELinux(); err == nil && exists {
		if err := selinux.RelabelFiles(util.UnprivilegedContainerSELinuxLabel, se.IsPermissive(), prSock); err != nil {
			return nil, fmt.Errorf("error relabeling required files: %v", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("failed to detect the presence of selinux: %v", err)
	}

	m := new(pluginapi.Mount)
	m.HostPath = dpi.socketDir
	m.ContainerPath = dpi.socketDir
	m.ReadOnly = false
	containerResponse.Mounts = []*pluginapi.Mount{m}

	response.ContainerResponses = []*pluginapi.ContainerAllocateResponse{containerResponse}

	return &response, nil
}

func (dpi *SocketDevicePlugin) registerServer() {
	pluginapi.RegisterDevicePluginServer(dpi.server, dpi)
}

func (dpi *SocketDevicePlugin) healthCheck() error {
	logger := log.DefaultLogger()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to creating a fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	devicePath := filepath.Join(dpi.socketDir, dpi.socket)

	// Start watching the files before we check for their existence to avoid races
	err = watcher.Add(dpi.socketDir)
	if err != nil {
		return fmt.Errorf("failed to add the device root path to the watcher: %v", err)
	}

	_, err = os.Stat(devicePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat the device: %v", err)
		}
		logger.Warningf("device '%s' is not present, the device plugin can't expose it.", dpi.socketName)
		dpi.health <- deviceHealth{Health: pluginapi.Unhealthy}
	}
	logger.Infof("device '%s' is present.", devicePath)

	dirName := filepath.Dir(dpi.socketPath)
	err = watcher.Add(dirName)

	if err != nil {
		return fmt.Errorf("failed to add the device-plugin kubelet path to the watcher: %v", err)
	}
	_, err = os.Stat(dpi.socketPath)
	if err != nil {
		return fmt.Errorf("failed to stat the device-plugin socket: %v", err)
	}

	for {
		select {
		case <-dpi.stop:
			return nil
		case err := <-watcher.Errors:
			logger.Reason(err).Errorf("error watching devices and device plugin directory")
		case event := <-watcher.Events:
			logger.V(4).Infof("health Event: %v", event)
			if event.Name == devicePath {
				// Health in this case is if the device path actually exists
				if event.Op == fsnotify.Create {
					logger.Infof("monitored device %s appeared", dpi.socketName)
					dpi.health <- deviceHealth{Health: pluginapi.Healthy}
				} else if (event.Op == fsnotify.Remove) || (event.Op == fsnotify.Rename) {
					logger.Infof("monitored device %s disappeared", dpi.socketName)
					dpi.health <- deviceHealth{Health: pluginapi.Unhealthy}
				}
			} else if event.Name == dpi.socketPath && event.Op == fsnotify.Remove {
				logger.Infof("device socket file for device %s was removed, kubelet probably restarted.", dpi.socketName)
				return nil
			}
		}
	}
}
