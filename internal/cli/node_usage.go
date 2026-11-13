package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

// nodeUsageOutput is a node with its last 24 hours of usage, so one answer
// carries both what the node used and what it has reserved and can offer.
type nodeUsageOutput struct {
	Node        *orchv1alpha1.Node        `json:"node"`
	StepSeconds uint32                    `json:"step_seconds"`
	Samples     []*orchv1alpha1.NodeUsage `json:"samples"`
}

func runNodeUsage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node usage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"-context": true, "--context": true})); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("node usage requires NODE and --context"), jsonOutput)
		return ExitInvalid
	}
	name := flags.Arg(0)
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	node, err := findNode(ctx, remote, name)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	history, err := remote.GetNodeUsage(ctx, name)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	output := nodeUsageOutput{Node: node, StepSeconds: history.GetStepSeconds(), Samples: history.GetSamples()}
	if output.Samples == nil {
		output.Samples = []*orchv1alpha1.NodeUsage{}
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, output)
	}
	printNodeUsage(stdout, output)
	return ExitOK
}

func printNodeUsage(stdout io.Writer, output nodeUsageOutput) {
	node := output.Node
	fmt.Fprintf(stdout, "%s  last 24 hours, %d samples\n", node.GetName(), len(output.Samples))
	var cpuPeak, cpuSum, memoryPeak, memorySum uint64
	for _, sample := range output.Samples {
		cpuPeak, cpuSum = max(cpuPeak, sample.GetCpuUsedMillis()), cpuSum+sample.GetCpuUsedMillis()
		memoryPeak, memorySum = max(memoryPeak, sample.GetMemoryUsedBytes()), memorySum+sample.GetMemoryUsedBytes()
	}
	count := max(uint64(len(output.Samples)), 1)
	usage := node.GetUsage()
	fmt.Fprintf(stdout, "  cpu     now=%dm avg=%dm peak=%dm reserved=%dm allocatable=%dm total=%dm\n",
		usage.GetCpuUsedMillis(), cpuSum/count, cpuPeak, node.GetAssignedCpuMillis(), node.GetAllocatableCpuMillis(), node.GetTotalCpuMillis())
	fmt.Fprintf(stdout, "  memory  now=%dB avg=%dB peak=%dB reserved=%dB allocatable=%dB total=%dB\n",
		usage.GetMemoryUsedBytes(), memorySum/count, memoryPeak, node.GetAssignedMemoryBytes(), node.GetAllocatableMemoryBytes(), node.GetTotalMemoryBytes())
	if usage.GetDiskTotalBytes() > 0 {
		fmt.Fprintf(stdout, "  disk    now=%dB total=%dB\n", usage.GetDiskUsedBytes(), usage.GetDiskTotalBytes())
	}
}
