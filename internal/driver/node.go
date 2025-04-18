package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/internal/driver/internal/k8s"
	"github.com/topolvm/topolvm/internal/filesystem"
	"github.com/topolvm/topolvm/internal/lvmd/command"
	"github.com/topolvm/topolvm/pkg/lvmd/proto"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mountutil "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	findmntCmd = "/bin/findmnt"
)

var nodeLogger = ctrl.Log.WithName("driver").WithName("node")

// NewNodeServer returns a new NodeServer.
func NewNodeServer(nodeName string, vgServiceClient proto.VGServiceClient, lvServiceClient proto.LVServiceClient, mgr manager.Manager) (csi.NodeServer, error) {
	lvService, err := k8s.NewLogicalVolumeService(mgr)
	if err != nil {
		return nil, err
	}

	return &nodeServer{
		server: &nodeServerNoLocked{
			nodeName:     nodeName,
			client:       vgServiceClient,
			lvService:    lvServiceClient,
			k8sLVService: lvService,
			mounter: mountutil.SafeFormatAndMount{
				Interface: mountutil.New(""),
				Exec:      utilexec.New(),
			},
		},
	}, nil
}

// This is a wrapper for nodeServerNoLocked to protect concurrent method calls.
type nodeServer struct {
	csi.UnimplementedNodeServer

	// This protects concurrent nodeServerNoLocked method calls.
	// We use a global lock because it assumes that each method does not take a long time,
	// and we scare about wired behaviors from concurrent device or filesystem operations.
	mu     sync.Mutex
	server *nodeServerNoLocked
}

func (s *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.server.NodePublishVolume(ctx, req)
}

func (s *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.server.NodeUnpublishVolume(ctx, req)
}

func (s *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.server.NodeGetVolumeStats(ctx, req)
}

func (s *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.server.NodeExpandVolume(ctx, req)
}

func (s *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	// This returns constant value only, it is unnecessary to take lock.
	return s.server.NodeGetCapabilities(ctx, req)
}

func (s *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	// This returns unmodified value only, it is unnecessary to take lock.
	return s.server.NodeGetInfo(ctx, req)
}

// nodeServerNoLocked implements csi.NodeServer.
// It does not take any lock, gRPC calls may be interleaved.
// Therefore, must not use it directly.
type nodeServerNoLocked struct {
	csi.UnimplementedNodeServer

	nodeName     string
	client       proto.VGServiceClient
	lvService    proto.LVServiceClient
	k8sLVService *k8s.LogicalVolumeService
	mounter      mountutil.SafeFormatAndMount
}

func (s *nodeServerNoLocked) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeContext := req.GetVolumeContext()
	volumeID := req.GetVolumeId()

	nodeLogger.Info("NodePublishVolume called",
		"volume_id", volumeID,
		"publish_context", req.GetPublishContext(),
		"target_path", req.GetTargetPath(),
		"volume_capability", req.GetVolumeCapability(),
		"read_only", req.GetReadonly(),
		"num_secrets", len(req.GetSecrets()),
		"volume_context", volumeContext)

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_id is provided")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no target_path is provided")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "no volume_capability is provided")
	}
	isBlockVol := req.GetVolumeCapability().GetBlock() != nil
	isFsVol := req.GetVolumeCapability().GetMount() != nil
	if !(isBlockVol || isFsVol) {
		return nil, status.Errorf(codes.InvalidArgument, "no supported volume capability: %v", req.GetVolumeCapability())
	}
	// we only support SINGLE_NODE_WRITER
	accessMode := req.GetVolumeCapability().GetAccessMode().GetMode()
	switch accessMode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
	default:
		modeName := csi.VolumeCapability_AccessMode_Mode_name[int32(accessMode)]
		return nil, status.Errorf(codes.FailedPrecondition, "unsupported access mode: %s (%d)", modeName, accessMode)
	}

	var lv *proto.LogicalVolume
	var err error

	lvr, err := s.k8sLVService.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	lv, err = s.getLvFromContext(ctx, lvr.Spec.DeviceClass, volumeID)
	if err != nil {
		return nil, err
	}
	if lv == nil {
		return nil, status.Errorf(codes.NotFound, "failed to find LV: %s", volumeID)
	}

	if isBlockVol {
		err = s.nodePublishBlockVolume(req, lv)
	} else if isFsVol {
		err = s.nodePublishFilesystemVolume(req, lv)
	}
	if err != nil {
		return nil, err
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func makeMountOptions(readOnly bool, mountOption *csi.VolumeCapability_MountVolume) ([]string, error) {
	mountOptions := make([]string, 0, len(mountOption.MountFlags)+2)
	mountOptions = append(mountOptions, toReadOnlyMountOption(readOnly)...)

	for _, f := range mountOption.MountFlags {
		if f == "rw" && readOnly {
			return nil, status.Error(codes.InvalidArgument, "mount option \"rw\" is specified even though read only mode is specified")
		}
		mountOptions = append(mountOptions, f)
	}

	// avoid duplicate UUIDs
	if mountOption.FsType == "xfs" {
		mountOptions = append(mountOptions, "nouuid")
	}

	return mountOptions, nil
}

func toReadOnlyMountOption(readOnly bool) []string {
	return map[bool][]string{true: {"ro"}, false: nil}[readOnly]
}

func (s *nodeServerNoLocked) nodePublishFilesystemVolume(req *csi.NodePublishVolumeRequest, lv *proto.LogicalVolume) error {
	// Check request
	mountOption := req.GetVolumeCapability().GetMount()
	if mountOption.FsType == "" {
		mountOption.FsType = "ext4"
	}
	mountOptions, err := makeMountOptions(req.GetReadonly(), mountOption)
	if err != nil {
		return err
	}

	err = os.MkdirAll(req.GetTargetPath(), 0755)
	if err != nil {
		return status.Errorf(codes.Internal, "mkdir failed: target=%s, error=%v", req.GetTargetPath(), err)
	}

	fsType, err := filesystem.DetectFilesystem(lv.GetPath())
	if err != nil {
		return status.Errorf(codes.Internal, "filesystem check failed: volume=%s, error=%v", req.GetVolumeId(), err)
	}

	if fsType != "" && fsType != mountOption.FsType {
		return status.Errorf(codes.Internal, "target device is already formatted with different filesystem: volume=%s, current=%s, new:%s", req.GetVolumeId(), fsType, mountOption.FsType)
	}

	mounted, err := s.mounter.IsMountPoint(req.GetTargetPath())
	if err != nil {
		return status.Errorf(codes.Internal, "mount check failed: target=%s, error=%v", req.GetTargetPath(), err)
	}

	if !mounted {
		mountFunc := s.mounter.FormatAndMount
		if len(fsType) > 0 {
			mountFunc = s.mounter.Mount
		}
		if err := mountFunc(lv.GetPath(), req.GetTargetPath(), mountOption.FsType, mountOptions); err != nil {
			return status.Errorf(codes.Internal, "mount failed: volume=%s, error=%v", req.GetVolumeId(), err)
		}
		if err := os.Chmod(req.GetTargetPath(), 0777|os.ModeSetgid); err != nil {
			return status.Errorf(codes.Internal, "chmod 2777 failed: target=%s, error=%v", req.GetTargetPath(), err)
		}
	}

	r := mountutil.NewResizeFs(s.mounter.Exec)
	if resize, err := r.NeedResize(lv.GetPath(), req.GetTargetPath()); resize {
		if _, err := r.Resize(lv.GetPath(), req.GetTargetPath()); err != nil {
			return status.Errorf(codes.Internal, "failed to resize filesystem %s (mounted at: %s): %v", req.VolumeId, req.GetTargetPath(), err)
		}
	} else if err != nil {
		return status.Errorf(codes.Internal, "could not determine if fs needed resize after mount: target=%s, error=%v", req.GetTargetPath(), err)
	}

	nodeLogger.Info("NodePublishVolume(fs) succeeded",
		"volume_id", req.GetVolumeId(),
		"target_path", req.GetTargetPath(),
		"fstype", mountOption.FsType)

	return nil
}

func (s *nodeServerNoLocked) nodePublishBlockVolume(req *csi.NodePublishVolumeRequest, lv *proto.LogicalVolume) error {
	// Find lv and create a block device with it
	// We mount via bind mount so that we can also respect the readonly flag
	mountOptions := append(toReadOnlyMountOption(req.GetReadonly()), "bind")

	fi, err := os.Create(req.GetTargetPath())
	if err != nil {
		return status.Errorf(codes.Internal, "create failed: target=%s, error=%v", req.GetTargetPath(), err)
	}
	defer func() { _ = fi.Close() }()

	if err := os.Chmod(req.GetTargetPath(), 0755); err != nil {
		return status.Errorf(codes.Internal, "chmod failed: target=%s, error=%v", req.GetTargetPath(), err)
	}

	if err := s.mounter.Mount(lv.GetPath(), req.GetTargetPath(), "", mountOptions); err != nil {
		return status.Errorf(codes.Internal, "(bind)mount failed: volume=%s, error=%v", req.GetVolumeId(), err)
	}

	nodeLogger.Info("NodePublishVolume(block) succeeded",
		"volume_id", req.GetVolumeId(),
		"target_path", req.GetTargetPath())
	return nil
}

func (s *nodeServerNoLocked) findVolumeByID(listResp *proto.GetLVListResponse, name string) *proto.LogicalVolume {
	for _, v := range listResp.Volumes {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func (s *nodeServerNoLocked) getLvFromContext(ctx context.Context, deviceClass, volumeID string) (*proto.LogicalVolume, error) {
	listResp, err := s.client.GetLVList(ctx, &proto.GetLVListRequest{DeviceClass: deviceClass})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list LV: %v", err)
	}
	return s.findVolumeByID(listResp, volumeID), nil
}

func (s *nodeServerNoLocked) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	nodeLogger.Info("NodeUnpublishVolume called",
		"volume_id", volumeID,
		"target_path", targetPath)

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_id is provided")
	}
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no target_path is provided")
	}

	info, err := os.Stat(targetPath)
	if os.IsNotExist(err) {
		// target_path does not exist, but legacy device for mount-type PV may still exist.
		_ = os.Remove(filepath.Join(topolvm.LegacyDeviceDirectory, volumeID))
		return &csi.NodeUnpublishVolumeResponse{}, nil
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "stat failed for %s: %v", targetPath, err)
	}

	// remove device file if target_path is device, unmount target_path otherwise
	if info.IsDir() {
		err = s.nodeUnpublishFilesystemVolume(req)
	} else {
		err = s.nodeUnpublishBlockVolume(req)
	}
	if err != nil {
		return nil, err
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *nodeServerNoLocked) nodeUnpublishFilesystemVolume(req *csi.NodeUnpublishVolumeRequest) error {
	targetPath := req.GetTargetPath()

	if err := mountutil.CleanupMountPoint(targetPath, s.mounter, true); err != nil {
		return status.Errorf(codes.Internal, "unmount failed for %s: error=%v", targetPath, err)
	}

	nodeLogger.Info("NodeUnpublishVolume(fs) is succeeded",
		"volume_id", req.GetVolumeId(),
		"target_path", targetPath)
	return nil
}

func (s *nodeServerNoLocked) nodeUnpublishBlockVolume(req *csi.NodeUnpublishVolumeRequest) error {
	targetPath := req.GetTargetPath()

	if err := mountutil.CleanupMountPoint(targetPath, s.mounter, true); err != nil {
		return status.Errorf(codes.Internal, "unmount failed for %s: error=%v", targetPath, err)
	}

	nodeLogger.Info("NodeUnpublishVolume(block) is succeeded",
		"volume_id", req.GetVolumeId(),
		"target_path", req.GetTargetPath())
	return nil
}

func (s *nodeServerNoLocked) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	nodeLogger.Info("NodeGetVolumeStats is called", "volume_id", volumeID, "volume_path", volumePath)
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_id is provided")
	}
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_path is provided")
	}

	var st unix.Stat_t
	switch err := filesystem.Stat(volumePath, &st); {
	case errors.Is(err, unix.ENOENT):
		return nil, status.Error(codes.NotFound, "Volume is not found at "+volumePath)
	case err == nil:
	default:
		return nil, status.Errorf(codes.Internal, "stat on %s was failed: %v", volumePath, err)
	}

	if (st.Mode & unix.S_IFMT) == unix.S_IFBLK {
		f, err := os.Open(volumePath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "open on %s was failed: %v", volumePath, err)
		}
		defer func() { _ = f.Close() }()
		pos, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "seek on %s was failed: %v", volumePath, err)
		}
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{{Total: pos, Unit: csi.VolumeUsage_BYTES}},
		}, nil
	}

	if st.Mode&unix.S_IFDIR == 0 {
		return nil, status.Errorf(codes.Internal, "invalid mode bits for %s: %d", volumePath, st.Mode)
	}

	var sfs unix.Statfs_t
	if err := filesystem.Statfs(volumePath, &sfs); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs on %s was failed: %v", volumePath, err)
	}

	var usage []*csi.VolumeUsage
	if sfs.Blocks > 0 {
		//nolint:unconvert // explicit conversion of Frsize for s390x.
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_BYTES,
			Total:     int64(sfs.Blocks) * int64(sfs.Frsize),
			Used:      int64(sfs.Blocks-sfs.Bfree) * int64(sfs.Frsize),
			Available: int64(sfs.Bavail) * int64(sfs.Frsize),
		})
	}
	if sfs.Files > 0 {
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_INODES,
			Total:     int64(sfs.Files),
			Used:      int64(sfs.Files - sfs.Ffree),
			Available: int64(sfs.Ffree),
		})
	}

	var lv *proto.LogicalVolume
	lvr, err := s.k8sLVService.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	lv, err = s.getLvFromContext(ctx, lvr.Spec.DeviceClass, volumeID)
	if err != nil {
		return nil, err
	}
	if lv == nil {
		return nil, status.Errorf(codes.NotFound, "failed to find LV: %s", volumeID)
	}

	volumeCondition, err := getVolumeCondition(lv)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeGetVolumeStatsResponse{Usage: usage, VolumeCondition: volumeCondition}, nil
}

func (s *nodeServerNoLocked) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	logger := nodeLogger.WithValues("volume_id", volumeID,
		"volume_path", volumePath,
		"required", req.GetCapacityRange().GetRequiredBytes(),
		"limit", req.GetCapacityRange().GetLimitBytes())

	logger.Info("NodeExpandVolume is called")

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_id is provided")
	}
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume_path is provided")
	}

	// We need to check the capacity range but don't use the converted value
	// because the filesystem can be resized without the requested size.
	_, err := convertRequestCapacityBytes(
		req.GetCapacityRange().GetRequiredBytes(),
		req.GetCapacityRange().GetLimitBytes(),
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if isBlock := req.GetVolumeCapability().GetBlock() != nil; isBlock {
		logger.Info("NodeExpandVolume(block) is skipped")
		return &csi.NodeExpandVolumeResponse{}, nil
	}

	lvr, err := s.k8sLVService.GetVolume(ctx, volumeID)
	deviceClass := topolvm.DefaultDeviceClassName
	if err == nil {
		deviceClass = lvr.Spec.DeviceClass
	} else if !errors.Is(err, k8s.ErrVolumeNotFound) {
		return nil, err
	}
	lv, err := s.getLvFromContext(ctx, deviceClass, volumeID)
	if err != nil {
		return nil, err
	}
	if lv == nil {
		return nil, status.Errorf(codes.NotFound, "failed to find LV: %s", volumeID)
	}

	args := []string{"-o", "source", "--noheadings", "--target", req.GetVolumePath()}
	output, err := s.mounter.Exec.Command(findmntCmd, args...).Output()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "findmnt error occurred: %v", err)
	}

	devicePath := strings.TrimSpace(string(output))
	if len(devicePath) == 0 {
		return nil, status.Errorf(codes.Internal, "filesystem %s is not mounted at %s", volumeID, volumePath)
	}
	logger = logger.WithValues("device", devicePath)

	logger.Info("triggering filesystem resize")
	r := mountutil.NewResizeFs(s.mounter.Exec)
	if _, err := r.Resize(lv.GetPath(), volumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resize filesystem %s (mounted at: %s): %v", volumeID, volumePath, err)
	}

	logger.Info("NodeExpandVolume(fs) is succeeded")

	// `capacity_bytes` in NodeExpandVolumeResponse is defined as OPTIONAL.
	// If this field needs to be filled, the value should be equal to `.status.currentSize` of the corresponding
	// `LogicalVolume`, but currently the node plugin does not have an access to the resource.
	// In addition to this, Kubernetes does not care if the field is blank or not, so leave it blank.
	return &csi.NodeExpandVolumeResponse{}, nil
}

func (s *nodeServerNoLocked) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	capabilities := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
	}

	csiCaps := make([]*csi.NodeServiceCapability, len(capabilities))
	for i, capability := range capabilities {
		csiCaps[i] = &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: capability,
				},
			},
		}
	}

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}, nil
}

func (s *nodeServerNoLocked) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeName,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				topolvm.GetTopologyNodeKey(): s.nodeName,
			},
		},
	}, nil
}

func getVolumeCondition(lv *proto.LogicalVolume) (*csi.VolumeCondition, error) {
	attr, err := command.ParsedLVAttr(lv.GetAttr())
	if err != nil {
		return nil, fmt.Errorf("failed to parse attributes returned from logical volume service: %w", err)
	}
	var volumeCondition csi.VolumeCondition
	if err := attr.VerifyHealth(); err != nil {
		volumeCondition = csi.VolumeCondition{
			Abnormal: true,
			Message:  err.Error(),
		}
	} else {
		volumeCondition = csi.VolumeCondition{
			Abnormal: false,
			Message:  "volume is healthy and operating normally",
		}
	}
	return &volumeCondition, nil
}
