//go:build !linux

package vitrual_manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	vcudaapi "tkestack.io/gpu-manager/pkg/api/runtime/vcuda"
	"tkestack.io/gpu-manager/pkg/config"
	"tkestack.io/gpu-manager/pkg/runtime"
	"tkestack.io/gpu-manager/pkg/services/response"
	"tkestack.io/gpu-manager/pkg/services/watchdog"
	"tkestack.io/gpu-manager/pkg/types"
	"tkestack.io/gpu-manager/pkg/utils"
)

//VirtualManager manages vGPUs
type VirtualManager struct {
	sync.Mutex

	cfg                     *config.Config
	containerRuntimeManager runtime.ContainerRuntimeInterface
	vDeviceServers          map[string]*grpc.Server
	responseManager         response.Manager
}

var _ vcudaapi.VCUDAServiceServer = &VirtualManager{}

//NewVirtualManager returns a new VirtualManager.
func NewVirtualManager(config *config.Config,
	runtimeManager runtime.ContainerRuntimeInterface,
	responseManager response.Manager) *VirtualManager {
	manager := &VirtualManager{
		cfg:                     config,
		containerRuntimeManager: runtimeManager,
		vDeviceServers:          make(map[string]*grpc.Server),
		responseManager:         responseManager,
	}

	return manager
}

//NewVirtualManagerForTest returns a new VirtualManager with fake docker
//client for testing.
func NewVirtualManagerForTest(config *config.Config,
	runtimeManager runtime.ContainerRuntimeInterface,
	responseManager response.Manager) *VirtualManager {
	manager := &VirtualManager{
		cfg:                     config,
		vDeviceServers:          make(map[string]*grpc.Server),
		containerRuntimeManager: runtimeManager,
		responseManager:         responseManager,
	}

	return manager
}

//Run starts a VirtualManager
func (vm *VirtualManager) Run() {
	if len(vm.cfg.VirtualManagerPath) == 0 {
		klog.Fatalf("Please set virtual manager path")
	}

	if err := os.MkdirAll(vm.cfg.VirtualManagerPath, DEFAULT_DIR_MODE); err != nil && !os.IsNotExist(err) {
		klog.Fatalf("can't create %s, error %s", vm.cfg.VirtualManagerPath, err)
	}

	registered := make(chan struct{})
	go vm.vDeviceWatcher(registered)
	<-registered

	go vm.garbageCollector()
	go vm.process()
	klog.V(2).Infof("Virtual manager is running")
}

func (vm *VirtualManager) vDeviceWatcher(registered chan struct{}) {
	klog.V(2).Infof("Start vDevice watcher")

	activePods := watchdog.GetActivePods()
	possibleActiveVm := vm.responseManager.ListAll()

	for uid, containerMapping := range possibleActiveVm {
		_, ok := activePods[uid]
		if !ok {
			continue
		}

		for name, resp := range containerMapping {
			dirName := utils.GetVirtualControllerMountPath(resp)
			if dirName == "" {
				klog.Errorf("can't find %s/%s allocResp", uid, name)
				continue
			}

			if _, err := os.Stat(dirName); err != nil {
				klog.V(2).Infof("Skip directory %s", dirName)
				continue
			}

			if len(filepath.Join(dirName, types.VDeviceSocket)) < 108 {
				srv := runVDeviceServer(dirName, vm)
				if srv == nil {
					klog.Fatalf("Can't recover vDevice server for %s", dirName)
				}

				klog.V(2).Infof("Recover vDevice server for %s", dirName)
				vm.Lock()
				vm.vDeviceServers[dirName] = srv
				vm.Unlock()
			} else {
				klog.Warningf("Ignore directory %s", dirName)
			}

			srv := runVDeviceServer(dirName, vm)
			if srv == nil {
				klog.Fatalf("Can't recover vDevice server for %s", dirName)
			}

			klog.V(2).Infof("Recover vDevice server for %s", dirName)
			vm.Lock()
			vm.vDeviceServers[dirName] = srv
			vm.Unlock()
		}
	}

	close(registered)

	wait.Forever(func() {
		vm.Lock()
		defer vm.Unlock()

		for dir, srv := range vm.vDeviceServers {
			_, err := os.Stat(dir)
			if err != nil && os.IsNotExist(err) {
				klog.V(2).Infof("Close orphaned server %s", dir)
				srv.Stop()
				delete(vm.vDeviceServers, dir)
			}
		}
	}, time.Minute)
}

func (vm *VirtualManager) garbageCollector() {
	klog.V(2).Infof("Starting garbage directory collector")
	wait.Forever(func() {
		needDeleted := make([]string, 0)

		activePods := watchdog.GetActivePods()
		possibleActiveVm := vm.responseManager.ListAll()

		for uid, containerMapping := range possibleActiveVm {
			if _, ok := activePods[uid]; !ok {
				for name, resp := range containerMapping {
					dirName := utils.GetVirtualControllerMountPath(resp)
					if dirName != "" {
						klog.Warningf("Find orphaned pod %s/%s", uid, name)
						needDeleted = append(needDeleted, dirName)
					}
				}
			}
		}

		for _, dir := range needDeleted {
			klog.V(2).Infof("Remove directory %s", dir)
			os.RemoveAll(filepath.Clean(dir))
		}
	}, time.Minute)
}

//                Host                     |                Container
//                                         |
//                                         |
//  .-----------.                          |
//  | allocator |----------.               |             ___________
//  '-----------'   PodUID |               |             \          \
//                         v               |              ) User App )--------.
//                .-----------------.      |             /__________/         |
//     .----------| virtual-manager |      |                                  |
//     |          '-----------------'      |                                  |
// $VirtualManagerPath/PodUID              |                                  |
//     |                                   |       read /proc/self/cgroup     |
//     |  .------------------.             |       to get PodUID, ContainerID |
//     '->| create directory |------.      |                                  |
//        '------------------'      |      |                                  |
//                                  |      |                                  |
//                 .----------------'      |       .----------------------.   |
//                 |                       |       | fork call gpu-client |<--'
//                 |                       |       '----------------------'
//                 v                       |                   |
//    .------------------------.           |                   |
//   ( wait for client register )<-------PodUID, ContainerID---'
//    '------------------------'           |
//                 |                       |
//                 v                       |
//   .--------------------------.          |
//   | locate pod and container |          |
//   '--------------------------'          |
//                 |                       |
//                 v                       |
//   .---------------------------.         |
//   | write down configure and  |         |
//   | pid file with containerID |         |
//   | as name                   |         |
//   '---------------------------'         |
//                                         |
//                                         |
//                                         v
func (vm *VirtualManager) process() {
	vcudaConfigFunc := func(podUID string) error {
		dirName := filepath.Clean(filepath.Join(vm.cfg.VirtualManagerPath, podUID))
		if err := os.MkdirAll(dirName, DEFAULT_DIR_MODE); err != nil && !os.IsExist(err) {
			return err
		}

		srv := runVDeviceServer(dirName, vm)
		if srv == nil {
			return fmt.Errorf("can't recover vDevice server for %s", dirName)
		}

		klog.V(2).Infof("Start vDevice server for %s", dirName)
		vm.Lock()
		vm.vDeviceServers[dirName] = srv
		vm.Unlock()

		return nil
	}

	klog.V(2).Infof("Starting process vm events")
	for evt := range vm.cfg.VCudaRequestsQueue {
		podUID := evt.PodUID
		klog.V(2).Infof("process %s", podUID)
		evt.Done <- vcudaConfigFunc(podUID)
	}
}

func (vm *VirtualManager) registerVDeviceWithContainerId(podUID, contID string) (*vcudaapi.VDeviceResponse, error) {
	klog.V(2).Infof("UID: %s, cont: %s want to registration", podUID, contID)

	containerInfo, err := vm.containerRuntimeManager.InspectContainer(contID)
	if err != nil {
		return nil, fmt.Errorf("can't find %s from docker", contID)
	}

	resp := vm.responseManager.GetResp(podUID, containerInfo.Metadata.Name)
	if resp == nil {
		return nil, fmt.Errorf("unable to load allocResp for %s/%s", podUID, contID)
	}

	baseDir := utils.GetVirtualControllerMountPath(resp)
	if baseDir == "" {
		return nil, fmt.Errorf("unable to find virtual manager controller path")
	}

	pidFilename := filepath.Join(baseDir, contID, PIDS_CONFIG_NAME)
	configFilename := filepath.Join(baseDir, contID, CONTROLLER_CONFIG_NAME)

	if err := os.MkdirAll(filepath.Dir(configFilename), DEFAULT_DIR_MODE); err != nil && !os.IsExist(err) {
		return nil, err
	}

	// write down pid file
	err = vm.writePidFile(pidFilename, contID)
	if err != nil {
		return nil, err
	}

	if err := vm.writeConfigFile(configFilename, podUID, containerInfo.Metadata.Name); err != nil {
		return nil, err
	}

	return &vcudaapi.VDeviceResponse{}, nil
}

// Deprecated
func (vm *VirtualManager) registerVDeviceWithContainerName(podUID, contName string) (*vcudaapi.VDeviceResponse, error) {
	klog.V(2).Infof("UID: %s, contName: %s want to registration", podUID, contName)

	resp := vm.responseManager.GetResp(podUID, contName)
	if resp == nil {
		return nil, fmt.Errorf("unable to load allocResp for %s/%s", podUID, contName)
	}

	baseDir := utils.GetVirtualControllerMountPath(resp)
	if baseDir == "" {
		return nil, fmt.Errorf("unable to find virtual manager controller path")
	}

	pidFilename := filepath.Join(baseDir, contName, PIDS_CONFIG_NAME)
	configFilename := filepath.Join(baseDir, contName, CONTROLLER_CONFIG_NAME)

	if err := os.MkdirAll(filepath.Dir(configFilename), DEFAULT_DIR_MODE); err != nil && !os.IsExist(err) {
		return nil, err
	}

	containerID := ""
	err := wait.Poll(time.Second, time.Minute, func() (done bool, err error) {
		activePods := watchdog.GetActivePods()
		pod, ok := activePods[podUID]
		if !ok {
			return false, fmt.Errorf("can't locate %s", podUID)
		}

		for _, stat := range pod.Status.ContainerStatuses {
			if strings.HasPrefix(stat.Name, contName) {
				containerID = stat.ContainerID
				break
			}
		}

		containerID = strings.TrimPrefix(containerID, "docker://")

		if len(containerID) == 0 {
			klog.Errorf("can't locate %s(%s)", podUID, contName)
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		return nil, err
	}

	if err := vm.writePidFile(pidFilename, containerID); err != nil {
		return nil, err
	}

	if err := vm.writeConfigFile(configFilename, podUID, contName); err != nil {
		return nil, err
	}

	return &vcudaapi.VDeviceResponse{}, nil
}

//RegisterVDevice handles RPC calls from vcuda client
func (vm *VirtualManager) RegisterVDevice(_ context.Context, req *vcudaapi.VDeviceRequest) (*vcudaapi.VDeviceResponse, error) {
	podUID := req.PodUid
	contName := req.ContainerName
	contID := req.ContainerId

	if len(contName) > 0 {
		return vm.registerVDeviceWithContainerName(podUID, contName)
	}

	return vm.registerVDeviceWithContainerId(podUID, contID)
}

func C_pids_to_disk(filename string, pids []int) error {
	return nil
}

func (vm *VirtualManager) writePidFile(filename string, contID string) error {
	klog.V(2).Infof("Write %s", filename)
	// cFileName := C.CString(filename)
	// defer C.free(unsafe.Pointer(cFileName))

	pidsInContainer, err := vm.containerRuntimeManager.GetPidsInContainers(contID)
	if err != nil {
		return err
	}
	if len(pidsInContainer) == 0 {
		return fmt.Errorf("empty pids")
	}
	// pids := make([]C.int, len(pidsInContainer))
	// for i := range pidsInContainer {
	// pids[i] = C.int(pidsInContainer[i])
	// }

	if C_pids_to_disk(filename, pidsInContainer); err != nil {
		return fmt.Errorf("can't sink pids file, %+v", err)
	}
	// if C.pids_to_disk(cFileName, &pids[0], (C.int)(len(pids))) != 0 {
	// 	return fmt.Errorf("can't sink pids file")
	// }

	return nil
}

func (vm *VirtualManager) writeConfigFile(filename string, podUID, name string) error {
	if _, err := os.Stat(filename); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		activePods := watchdog.GetActivePods()
		pod, ok := activePods[podUID]
		if !ok {
			return fmt.Errorf("can't locate %s", podUID)
		}

		hasLimitCore := false
		limitCores := 100

		if pod.Annotations != nil {
			limitData, ok := pod.Annotations[types.VCoreLimitAnnotation]
			if ok {
				hasLimitCore = true
				limit, err := strconv.Atoi(limitData)
				if err != nil {
					return err
				}

				if limit < limitCores {
					limitCores = limit
				}
			}
		}

		found := false
		for _, cont := range pod.Spec.Containers {
			if cont.Name == name || strings.HasPrefix(name, utils.MakeContainerNamePrefix(cont.Name)) {
				found = true
				coresLimit := cont.Resources.Limits[types.VCoreAnnotation]
				cores := (&coresLimit).Value()
				memoryLimit := cont.Resources.Limits[types.VMemoryAnnotation]
				memory := (&memoryLimit).Value() * types.MemoryBlockSize

				if err := func() error {
					// TODO
					fmt.Sprintf("%v,%v,%v", hasLimitCore, cores, memory)
					// var vcudaConfig C.struct_resource_data_t

					// cPodUID := C.CString(podUID)
					// cContName := C.CString(name)
					// cFileName := C.CString(filename)

					// defer C.free(unsafe.Pointer(cPodUID))
					// defer C.free(unsafe.Pointer(cContName))
					// defer C.free(unsafe.Pointer(cFileName))

					// C.strcpy(&vcudaConfig.pod_uid[0], (*C.char)(unsafe.Pointer(cPodUID)))
					// C.strcpy(&vcudaConfig.container_name[0], (*C.char)(unsafe.Pointer(cContName)))
					// vcudaConfig.gpu_memory = C.uint64_t(memory)
					// vcudaConfig.utilization = C.int(cores)
					// vcudaConfig.hard_limit = 1
					// vcudaConfig.driver_version.major = C.int(types.DriverVersionMajor)
					// vcudaConfig.driver_version.minor = C.int(types.DriverVersionMinor)

					// if cores >= nvidia.HundredCore {
					// 	vcudaConfig.enable = 0
					// } else {
					// 	vcudaConfig.enable = 1
					// }

					// if hasLimitCore {
					// 	vcudaConfig.hard_limit = 0
					// 	vcudaConfig.limit = C.int(limitCores)
					// }

					// if C.setting_to_disk(cFileName, &vcudaConfig) != 0 {
					// 	return fmt.Errorf("can't sink config %s", filename)
					// }

					return nil
				}(); err != nil {
					return err
				}
			}
		}

		if !found {
			return fmt.Errorf("can't locate %s(%s)", podUID, name)
		}
	}

	return nil
}

func runVDeviceServer(dir string, handler vcudaapi.VCUDAServiceServer) *grpc.Server {
	socketFile := filepath.Join(dir, types.VDeviceSocket)
	err := syscall.Unlink(socketFile)
	if err != nil && !os.IsNotExist(err) {
		klog.Errorf("remove %s failed, error %s", socketFile, err)
		return nil
	}

	l, err := net.Listen("unix", socketFile)
	if err != nil {
		klog.Errorf("listen %s failed, error %s", socketFile, err)
		return nil
	}

	if err := os.Chmod(socketFile, DEFAULT_DIR_MODE); err != nil {
		klog.Errorf("chmod %s failed, %v", socketFile, err)
		return nil
	}

	srv := grpc.NewServer()
	vcudaapi.RegisterVCUDAServiceServer(srv, handler)

	ch := make(chan error, 1)
	ready := make(chan struct{})

	go func() {
		close(ready)
		ch <- srv.Serve(l)
	}()

	<-ready

	select {
	case err := <-ch:
		klog.Errorf("start vDevice server failed, error %s", err)
		return nil
	default:
	}

	return srv
}
