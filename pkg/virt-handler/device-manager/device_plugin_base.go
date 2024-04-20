package device_manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"kubevirt.io/client-go/log"

	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

type DevicePluginBase struct {
	devs         []*pluginapi.Device
	server       *grpc.Server
	socketPath   string
	stop         <-chan struct{}
	health       chan deviceHealth
	registerFunc registerFunc
	resourceName string
	done         chan struct{}
	initialized  bool
	lock         *sync.Mutex
	deregistered chan struct{}
	devicePath   string
	deviceRoot   string
	deviceName   string
	healthcheck  healthFunc
}

func (dpi *DevicePluginBase) Start(stop <-chan struct{}) (err error) {
	log.DefaultLogger().Infof("ammar: starting device with info : %s, root: %s, path: %s, name: %s", dpi.socketPath, dpi.deviceRoot, dpi.devicePath, dpi.resourceName)
	dpi.stop = stop
	if err = dpi.cleanup(); err != nil {
		return err
	}

	sock, err := net.Listen("unix", dpi.socketPath)
	if err != nil {
		return fmt.Errorf("error creating GRPC server socket: %v", err)
	}

	dpi.server = grpc.NewServer([]grpc.ServerOption{}...)
	defer dpi.stopDevicePlugin()

	dpi.registerFunc()

	errChan := make(chan error, 2)

	go func() {
		errChan <- dpi.server.Serve(sock)
	}()

	err = waitForGRPCServer(dpi.socketPath, connectionTimeout)
	if err != nil {
		return fmt.Errorf("error starting the GRPC server: %v", err)
	}

	err = dpi.register()
	if err != nil {
		return fmt.Errorf("error registering with device plugin manager: %v", err)
	}

	go func() {
		errChan <- dpi.healthcheck() // this method will be called by whoever calls start
	}()

	dpi.setInitialized(true)
	err = <-errChan

	return err
}

func (dpi *DevicePluginBase) GetDeviceName() string {
	return dpi.resourceName
}

func (dpi *DevicePluginBase) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs})
	log.DefaultLogger().Infof("ammar: starting list and watch with devices %s", dpi.devs)

	done := false
	for {
		select {
		case devHealth := <-dpi.health:
			for _, dev := range dpi.devs {
				if devHealth.DevId == dev.ID {
					dev.Health = devHealth.Health
				}
			}
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs}); err != nil {
				log.DefaultLogger().Infof("an error occured, %s", err.Error())
			}
		case <-dpi.stop:
			done = true
		case <-dpi.done:
			done = true
		}
		if done {
			break
		}
	}
	// Send empty list to increase the chance that the kubelet acts fast on stopped device plugins
	// There exists no explicit way to deregister devices
	emptyList := []*pluginapi.Device{}
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: emptyList}); err != nil {
		log.DefaultLogger().Reason(err).Infof("%s device plugin failed to deregister", dpi.resourceName)
	}
	close(dpi.deregistered)
	return nil
}

func (dpi *DevicePluginBase) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	res := &pluginapi.PreStartContainerResponse{}
	return res, nil
}

func (dpi *DevicePluginBase) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}
	return options, nil
}

func (dpi *DevicePluginBase) stopDevicePlugin() error {
	defer func() {
		if !IsChanClosed(dpi.done) {
			close(dpi.done)
		}
	}()

	// Give the device plugin one second to properly deregister
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	select {
	case <-dpi.deregistered:
	case <-ticker.C:
	}

	dpi.server.Stop()
	dpi.setInitialized(false)
	return dpi.cleanup()
}

func (dpi *DevicePluginBase) cleanup() error {
	if err := os.Remove(dpi.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (dpi *DevicePluginBase) GetInitialized() bool {
	dpi.lock.Lock()
	defer dpi.lock.Unlock()
	return dpi.initialized
}

func (dpi *DevicePluginBase) setInitialized(initialized bool) {
	dpi.lock.Lock()
	dpi.initialized = initialized
	dpi.lock.Unlock()
}

func (dpi *DevicePluginBase) register() error {
	conn, err := gRPCConnect(pluginapi.KubeletSocket, connectionTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(dpi.socketPath),
		ResourceName: dpi.resourceName,
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}
