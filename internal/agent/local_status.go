package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/model"
)

const LocalStatusFilename = "status.json"

// LocalStatus is a private, ephemeral readiness receipt. It contains no secrets.
type LocalStatus struct {
	NodeID     string    `json:"node_id"`
	ClusterID  string    `json:"cluster_id"`
	PID        int       `json:"pid"`
	BootID     string    `json:"boot_id"`
	UpdatedAt  time.Time `json:"updated_at"`
	Connected  bool      `json:"connected"`
	Ready      bool      `json:"ready"`
	Generation uint64    `json:"assignment_generation"`
	Ingress    bool      `json:"ingress"`
}

func (r *sessionRunner) writeLocalStatus(report *orchv1alpha1.AllocationReport) error {
	if r.dataDir == "" {
		return nil
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	status := LocalStatus{NodeID: r.credentials.Config.NodeID, ClusterID: r.credentials.Config.ClusterID, PID: os.Getpid(), BootID: strings.TrimSpace(string(boot)), UpdatedAt: currentTime(r.now), Connected: r.assignmentReceived}
	if report != nil && r.assignmentReceived {
		status.Generation = report.AssignmentGeneration
		n := report.Network
		a, err := r.cache.Assignment(context.Background())
		if err == nil && n != nil {
			status.Ingress = a.Network.IngressEnabled
			status.Ready = assignmentReady(a, report)
		}
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return writePrivateAtomic(filepath.Join(r.dataDir, LocalStatusFilename), append(data, '\n'))
}
func ReadLocalStatus(dir string, now time.Time) (LocalStatus, error) {
	var status LocalStatus
	if err := requirePrivateDirectory(dir); err != nil {
		return status, err
	}
	data, err := readPrivateFile(filepath.Join(dir, LocalStatusFilename))
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return status, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return status, err
	}
	if status.BootID == "" || status.BootID != strings.TrimSpace(string(boot)) || now.Sub(status.UpdatedAt) > 15*time.Second || status.UpdatedAt.After(now.Add(time.Second)) {
		return status, fmt.Errorf("agent readiness receipt is stale")
	}
	return status, nil
}

func assignmentReady(a model.Assignment, report *orchv1alpha1.AllocationReport) bool {
	if report == nil || report.Network == nil {
		return false
	}
	n := report.Network
	return a.Generation == report.AssignmentGeneration && n.Generation == a.Network.Generation && n.State == orchv1alpha1.NetworkState_NETWORK_STATE_READY && n.DnsState == model.SubsystemReady && (!a.Network.IngressEnabled || n.IngressState == model.SubsystemReady)
}
