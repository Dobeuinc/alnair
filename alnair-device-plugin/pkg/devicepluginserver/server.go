package devicepluginserver

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	vs "alnair-device-plugin/pkg/vgpuserver"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	dockertypes "github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	"google.golang.org/grpc"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	serverSocketGPUMemory  = "alnair-gpu-mem.sock"
	resourceNameGPUMemory  = "alnair/vgpu-memory"
	serverSocketGPUCompute = "alnair-gpu-compute.sock"
	resourceNameGPUCompute = "alnair/vgpu-compute"
	gpuMemoryChunkSize     = 1073741824 // GiB
	alnairInterposeLibPath = "/opt/alnair/libcuinterpose.so"
)

type resourceType int

const (
	memory resourceType = iota
	compute
)

// StartDevicePluginServers starts both GPU memory and GPU compute device plugin servers
func StartDevicePluginServers() error {
	if err := nvml.Init(); err != nil {
		return err
	}

	gpuMemServerImpl := &GPUMemoryDPServer{
		stop: make(chan interface{}),
	}

	gpuMemServer := DevicePluginServer{
		socketName:   serverSocketGPUMemory,
		resourceName: resourceNameGPUMemory,
		serverImpl:   gpuMemServerImpl,
		grpcServer:   grpc.NewServer(),
		stop:         gpuMemServerImpl.stop,
	}

	if err := gpuMemServer.Start(); err != nil {
		return err
	}

	gpuComputeServerImpl := &GPUComputeDPServer{
		stop: make(chan interface{}),
	}

	gpuComputeServer := DevicePluginServer{
		socketName:   serverSocketGPUCompute,
		resourceName: resourceNameGPUCompute,
		serverImpl:   gpuComputeServerImpl,
		grpcServer:   grpc.NewServer(),
		stop:         gpuComputeServerImpl.stop,
	}

	if err := gpuComputeServer.Start(); err != nil {
		return err
	}

	go alnairWorkspaceRecycle()

	//only call once during the initalization process, patch static GPU info to the Node annotations
	err := PatchNode()
	if err != nil {
		return err
	}
	return nil
}

// DevicePluginServer encapsulates all the information to run a device plugin server for a single resource
type DevicePluginServer struct {
	socketName   string
	resourceName string
	serverImpl   pluginapi.DevicePluginServer
	grpcServer   *grpc.Server
	stop         chan interface{}
}

func (s *DevicePluginServer) Start() error {
	// Start grpc server
	sock := path.Join(pluginapi.DevicePluginPath, s.socketName)

	if err := os.RemoveAll(sock); err != nil && err != os.ErrNotExist {
		return err
	}

	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(s.grpcServer, s.serverImpl)
	go func() {
		if err := s.grpcServer.Serve(l); err != nil {
			log.Fatalf("failed to serve grpc: %v", err)
		}
	}()

	conn, err := dialGrpc(sock)
	if err != nil {
		log.Println("failed to wait for grpc server to be ready")
		return err
	}
	conn.Close()

	// register with kubelet
	if err := s.registerWithKubelet(); err != nil {
		log.Println("failed to register with kubelet")
		return err
	}

	return nil
}

func (s *DevicePluginServer) Stop() {
	close(s.stop)
	s.grpcServer.Stop()
}

func (s *DevicePluginServer) registerWithKubelet() error {
	conn, err := dialGrpc(pluginapi.KubeletSocket)
	if err != nil {
		log.Println("failed to dail kubelet grpc endpoint")
		return err
	}

	client := pluginapi.NewRegistrationClient(conn)
	request := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     s.socketName,
		ResourceName: s.resourceName,
		Options: &pluginapi.DevicePluginOptions{
			GetPreferredAllocationAvailable: true,
		},
	}

	if _, err = client.Register(context.Background(), request); err != nil {
		return err
	}
	log.Printf("register device %s to kubelet successfully.\n", s.resourceName)
	return nil
}

// GPUMemoryDPServer implements the device plugin server for vGPU memory
type GPUMemoryDPServer struct {
	pluginapi.UnimplementedDevicePluginServer
	stop chan interface{}
}

func (s *GPUMemoryDPServer) ListAndWatch(e *pluginapi.Empty, lws pluginapi.DevicePlugin_ListAndWatchServer) error {
	devs := getDevices(memory)
	lws.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	<-s.stop
	return nil
}

func (s *GPUMemoryDPServer) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var resp pluginapi.AllocateResponse
	var totalDeviceIDs []string
	totalDeviceCnt := 0
	for _, creq := range req.ContainerRequests {
		devIDs := getRealDeviceIDs(creq.DevicesIDs)

		alnairID := utilrand.String(5)
		hostWorkspacePath := path.Join(vs.AlnairContainerWorkspaceRoot, alnairID)
		if err := os.MkdirAll(hostWorkspacePath, 0700); err != nil {
			log.Printf("ERROR: failed to create alnair workspace %s: %v", hostWorkspacePath, err)
		}
		limitsFilepath := path.Join(hostWorkspacePath, "limits")
		limits := fmt.Sprintf("vmem:%v", len(creq.DevicesIDs)*gpuMemoryChunkSize)
		if err := os.WriteFile(limitsFilepath, []byte(limits), 0644); err != nil {
			log.Printf("ERROR: failed to write alnair resource limits")
		}

		var cresp pluginapi.ContainerAllocateResponse
		cresp.Envs = map[string]string{
			"NVIDIA_VISIBLE_DEVICES": strings.Join(devIDs, ","),
			"ALNAIR_ID":              alnairID,
			"ALNAIR_WORKSPACE_PATH":  vs.AlnairContainerWorkspaceRoot,
			"ALNAIR_SOCKET":          vs.AlnairCgroupServerSocket,
			"LD_PRELOAD":             alnairInterposeLibPath,
		}
		cresp.Mounts = []*pluginapi.Mount{
			{
				ContainerPath: vs.AlnairContainerWorkspaceRoot,
				HostPath:      hostWorkspacePath,
			},
			{
				ContainerPath: alnairInterposeLibPath,
				HostPath:      alnairInterposeLibPath,
			},
			{
				ContainerPath: vs.AlnairCgroupServerSocket,
				HostPath:      vs.AlnairCgroupServerSocket,
			},
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &cresp)
		totalDeviceCnt += len(creq.DevicesIDs)
		totalDeviceIDs = append(totalDeviceIDs, creq.DevicesIDs...)

	}
	PatchPod(strings.Join(totalDeviceIDs[:], ","), totalDeviceCnt)
	return &resp, nil
}

func (s *GPUMemoryDPServer) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	var ret pluginapi.PreferredAllocationResponse
	for _, creq := range req.ContainerRequests {
		preferredDeviceIDs := getPreferredDeviceIDs(creq.AvailableDeviceIDs, creq.AllocationSize)
		ret.ContainerResponses = append(ret.ContainerResponses,
			&pluginapi.ContainerPreferredAllocationResponse{
				DeviceIDs: preferredDeviceIDs,
			},
		)
	}

	return &ret, nil
}

func (s *GPUMemoryDPServer) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: true,
	}, nil
}

// GPUComputeDPServer implements the device plugin server for GPU memory
type GPUComputeDPServer struct {
	pluginapi.UnimplementedDevicePluginServer
	stop chan interface{}
}

func (s *GPUComputeDPServer) ListAndWatch(e *pluginapi.Empty, lws pluginapi.DevicePlugin_ListAndWatchServer) error {
	devs := getDevices(compute)
	lws.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	<-s.stop
	return nil
}

func (s *GPUComputeDPServer) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var resp pluginapi.AllocateResponse
	for _, creq := range req.ContainerRequests {
		var cresp pluginapi.ContainerAllocateResponse

		computePerc := len(creq.DevicesIDs)
		if computePerc > 100 {
			computePerc = 100
		}

		cresp.Envs = map[string]string{
			"ALNAIR_VGPU_COMPUTE_PERCENTILE": fmt.Sprintf("%d", computePerc),
		}

		resp.ContainerResponses = append(resp.ContainerResponses, &cresp)
	}
	return &resp, nil
}

func (s *GPUComputeDPServer) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: true,
	}, nil
}

func (s *GPUComputeDPServer) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	var ret pluginapi.PreferredAllocationResponse
	for _, creq := range req.ContainerRequests {
		preferredDeviceIDs := getPreferredDeviceIDs(creq.AvailableDeviceIDs, creq.AllocationSize)
		ret.ContainerResponses = append(ret.ContainerResponses,
			&pluginapi.ContainerPreferredAllocationResponse{
				DeviceIDs: preferredDeviceIDs,
			},
		)
	}

	return &ret, nil
}

func getRealDeviceIDs(syntheticIDs []string) []string {
	var ret []string
	sort.Strings(syntheticIDs)
	for _, sid := range syntheticIDs {
		id := strings.SplitN(sid, "_", 2)[0]
		if len(ret) == 0 || id != ret[len(ret)-1] {
			ret = append(ret, id)
		}
	}
	return ret
}

func getPreferredDeviceIDs(availableDeviceIDs []string, allocationSize int32) []string {
	//preferred devices are all the vGPUs from the same physcial ones,
	//sort the device ID and scan the prefix, make sure prefix are the same
	sort.Strings(availableDeviceIDs) //sort DeviceID, so that return devices are likely to have the same prefix (phyical GPU ID)
	startIdx := int32(0)
	notFound := true
	for notFound && (startIdx+allocationSize <= int32(len(availableDeviceIDs))) {
		notFound = false
		firstId := strings.SplitN(availableDeviceIDs[startIdx], "_", 2)[0]
		var id string
		for i := startIdx; i < startIdx+allocationSize; i++ {
			id = strings.SplitN(availableDeviceIDs[i], "_", 2)[0]
			if id != firstId {
				log.Println("reach the corner, start from a new gpu")
				startIdx = i //restart checking from the next physical card
				notFound = true
				break
			}
		}

	}
	if notFound { //just pick what we have, but this should not happen, scheduler is supposed to filter this node out
		startIdx = 0
		log.Println("warning: cannot find enough vGPU within one phyiscal GPU, assign mixed vGPUs", "available vGPU:", len(availableDeviceIDs), "request count:", allocationSize)
	}
	return availableDeviceIDs[startIdx : startIdx+allocationSize]
}

func getDevices(t resourceType) []*pluginapi.Device {
	n, err := nvml.GetDeviceCount()
	if err != nil {
		panic(err)
	}

	var devs []*pluginapi.Device
	for i := uint(0); i < n; i++ {
		d, err := nvml.NewDevice(i)
		if err != nil {
			panic(err)
		}

		if t == memory {
			devs = append(devs, getPluginApiMemoryDevice(d)...)
		} else {
			devs = append(devs, getPluginApiComputeDevice(d)...)
		}
	}

	return devs
}

func GetPhysicalDeivces() (n uint, uuids []string, vDeviceSize []string) {
	n, err := nvml.GetDeviceCount()
	if err != nil {
		panic(err)
	}
	for i := uint(0); i < n; i++ {
		d, err := nvml.NewDevice(i)
		if err != nil {
			panic(err)
		}
		uuids = append(uuids, d.UUID)
		vDevices := getPluginApiMemoryDevice(d)
		vDeviceSize = append(vDeviceSize, fmt.Sprint(len(vDevices)))
	}
	return n, uuids, vDeviceSize
}

func getPluginApiMemoryDevice(d *nvml.Device) []*pluginapi.Device {
	var ret []*pluginapi.Device
	chunkSzInMiB := gpuMemoryChunkSize / 1024 / 1024
	numChunks := (int(*d.Memory) + chunkSzInMiB/2) / chunkSzInMiB
	for i := uint(0); i < uint(numChunks); i++ {
		var t pluginapi.Device
		t.ID = fmt.Sprintf("%s_%d", d.UUID, i)
		t.Health = pluginapi.Healthy
		if d.CPUAffinity != nil {
			t.Topology = &pluginapi.TopologyInfo{
				Nodes: []*pluginapi.NUMANode{
					{ID: int64(*d.CPUAffinity)},
				},
			}
		}
		ret = append(ret, &t)
	}
	return ret
}

func getPluginApiComputeDevice(d *nvml.Device) []*pluginapi.Device {
	var ret []*pluginapi.Device
	for i := 0; i < 100; i++ {
		var t pluginapi.Device
		t.ID = fmt.Sprintf("%s_%d", d.UUID, i)
		t.Health = pluginapi.Healthy
		ret = append(ret, &t)
	}
	return ret
}

func dialGrpc(sock string) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(
		ctx,
		"unix://"+sock,
		grpc.WithInsecure(),
		grpc.WithBlock(),
	)

	if err != nil {
		return nil, err
	}

	return conn, nil
}

func alnairWorkspaceRecycle() {
	for {
		dentries, err := ioutil.ReadDir(vs.AlnairContainerWorkspaceRoot)
		if err != nil {
			log.Printf("cannot read workspace root dir: %v", vs.AlnairContainerWorkspaceRoot)
			goto sleep
		}

		for _, de := range dentries {
			if !de.IsDir() {
				continue
			}

			filepath := path.Join(vs.AlnairContainerWorkspaceRoot, de.Name(), "containerID")

			if _, err := os.Stat(filepath); os.IsNotExist(err) {
				continue
			}

			in, err := ioutil.ReadFile(filepath)

			if err != nil {
				continue
			}

			containerID := string(in)

			if !containerExists(containerID) {
				os.RemoveAll(path.Join(vs.AlnairContainerWorkspaceRoot, de.Name()))
			}
		}
	sleep:
		time.Sleep(10 * time.Second)
	}
}

func containerExists(containerID string) bool {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv)
	if err != nil {
		panic(err)
	}

	containers, err := cli.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		panic(err)
	}

	for _, container := range containers {
		if container.ID == containerID {
			return true
		}
		fmt.Printf("%s %s\n", container.ID[:10], container.Image)
	}

	return false
}
