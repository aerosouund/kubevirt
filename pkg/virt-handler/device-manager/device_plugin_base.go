package device_manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"kubevirt.io/client-go/log"

	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

type DevicePluginBase struct {
	devs       []*pluginapi.Device
	server     *grpc.Server
	socketPath string
	stop       <-chan struct{}
	health     chan deviceHealth
	// allocfunc    allocateFunc
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
	logger := log.DefaultLogger()
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
		logger.Info("ammar: dpi server wrote to the error channel")
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
		logger.Info("ammar: health wrote to the error channel")
	}()

	dpi.setInitialized(true)
	logger.Infof("ammar: %s device plugin started", dpi.resourceName)
	err = <-errChan
	logger.Info("ammar: we are not stuck on reading from the error channel")

	return err
}

func (dpi *DevicePluginBase) GetDeviceName() string {
	return dpi.resourceName
}

// func (dpi *DevicePluginBase) extraStart(errChan chan error, sock net.Listener) error {
// 	logger := log.DefaultLogger()
// 	pluginapi.RegisterDevicePluginServer(dpi.server, dpi)
// 	defer dpi.stopDevicePlugin()

// 	go func() {
// 		errChan <- dpi.server.Serve(sock)
// 		logger.Info("ammar: dpi server wrote to the error channel")
// 	}()

// 	err := waitForGRPCServer(dpi.socketPath, connectionTimeout)
// 	if err != nil {
// 		return fmt.Errorf("error starting the GRPC server: %v", err)
// 	}

// 	err = dpi.register()
// 	if err != nil {
// 		return fmt.Errorf("error registering with device plugin manager: %v", err)
// 	}

// 	go func() {
// 		errChan <- dpi.healthcheck() // this method will be called by whoever calls start
// 		logger.Info("ammar: health wrote to the error channel")
// 	}()

// 	dpi.setInitialized(true)
// 	logger.Infof("%s device plugin started", dpi.resourceName)
// 	err = <-errChan
// 	logger.Info("ammar: we are not stuck on reading from the error channel on the plugin itself")

// 	return err
// }

func (dpi *DevicePluginBase) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs})
	log.DefaultLogger().Infof("ammar: starting list and watch with devices %s", dpi.devs)

	done := false
	for {
		select {
		case devHealth := <-dpi.health:
			log.DefaultLogger().Infof("ammar: received a health event")
			for _, dev := range dpi.devs {
				if devHealth.DevId == dev.ID {
					dev.Health = devHealth.Health
				}
			}
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs}); err != nil {
				log.DefaultLogger().Infof("ammar: an error occured, %s", err.Error())
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

func (dpi *DevicePluginBase) healthCheck() error {
	logger := log.DefaultLogger()
	monitoredDevices := make(map[string]string)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to creating a fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	// This way we don't have to mount /dev from the node
	devicePath := filepath.Join(dpi.deviceRoot, dpi.devicePath)
	logger.Infof("ammar: devpath %s", devicePath)

	// Start watching the files before we check for their existence to avoid races
	dirName := filepath.Dir(devicePath)
	err = watcher.Add(dirName)
	if err != nil {
		return fmt.Errorf("failed to add the device root path to the watcher: %v", err)
	}

	_, err = os.Stat(devicePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat the device: %v", err)
		}
	}

	// probe all devices
	for _, dev := range dpi.devs {
		vfioDevice := filepath.Join(devicePath, dev.ID)
		logger.Infof("ammar: vfio device %s", vfioDevice)
		err = watcher.Add(vfioDevice)
		if err != nil {
			return fmt.Errorf("failed to add the device %s to the watcher: %v", vfioDevice, err)
		}
		monitoredDevices[vfioDevice] = dev.ID
	}

	dirName = filepath.Dir(dpi.socketPath)
	logger.Infof("ammar: socket %s", devicePath)
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
			if monDevId, exist := monitoredDevices[event.Name]; exist {
				// Health in this case is if the device path actually exists
				if event.Op == fsnotify.Create {
					logger.Infof("monitored device %s appeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Healthy,
					}
				} else if (event.Op == fsnotify.Remove) || (event.Op == fsnotify.Rename) {
					logger.Infof("monitored device %s disappeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Unhealthy,
					}
				}
			} else if event.Name == dpi.socketPath && event.Op == fsnotify.Remove {
				logger.Infof("device socket file for device %s was removed, kubelet probably restarted.", dpi.resourceName)
				return nil
			}
		case <-time.After(2 * time.Second):
			logger.Infof("ammar: No event received for 2 seconds from the watcher")
		}
	}
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

// func (dpi *DevicePluginBase) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
// 	return dpi.allocfunc(ctx, r)
// }

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
