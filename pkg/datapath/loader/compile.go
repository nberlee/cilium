// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package loader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/pkg/command/exec"
	"github.com/cilium/cilium/pkg/common"
	"github.com/cilium/cilium/pkg/datapath/linux/probes"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
)

// OutputType determines the type to be generated by the compilation steps.
type OutputType string

const (
	outputObject   = OutputType("obj")
	outputAssembly = OutputType("asm")
	outputSource   = OutputType("c")

	compiler = "clang"

	endpointPrefix   = "bpf_lxc"
	endpointProg     = endpointPrefix + "." + string(outputSource)
	endpointObj      = endpointPrefix + ".o"
	endpointObjDebug = endpointPrefix + ".dbg.o"
	endpointAsm      = endpointPrefix + "." + string(outputAssembly)

	hostEndpointPrefix       = "bpf_host"
	hostEndpointNetdevPrefix = "bpf_netdev_"
	hostEndpointProg         = hostEndpointPrefix + "." + string(outputSource)
	hostEndpointObj          = hostEndpointPrefix + ".o"
	hostEndpointObjDebug     = hostEndpointPrefix + ".dbg.o"
	hostEndpointAsm          = hostEndpointPrefix + "." + string(outputAssembly)

	networkPrefix = "bpf_network"
	networkProg   = networkPrefix + "." + string(outputSource)
	networkObj    = networkPrefix + ".o"

	xdpPrefix = "bpf_xdp"
	xdpProg   = xdpPrefix + "." + string(outputSource)
	xdpObj    = xdpPrefix + ".o"

	overlayPrefix = "bpf_overlay"
	overlayProg   = overlayPrefix + "." + string(outputSource)
	overlayObj    = overlayPrefix + ".o"
)

var (
	probeCPUOnce sync.Once

	// default fallback
	nameBPFCPU = "v1"
)

// progInfo describes a program to be compiled with the expected output format
type progInfo struct {
	// Source is the program source (base) filename to be compiled
	Source string
	// Output is the expected (base) filename produced from the source
	Output string
	// OutputType to be created by LLVM
	OutputType OutputType
	// Options are passed directly to LLVM as individual parameters
	Options []string
}

// directoryInfo includes relevant directories for compilation and linking
type directoryInfo struct {
	// Library contains the library code to be used for compilation
	Library string
	// Runtime contains headers for compilation
	Runtime string
	// State contains node, lxc, and features headers for templatization
	State string
	// Output is the directory where the files will be stored
	Output string
}

var (
	standardCFlags = []string{"-O2", "--target=bpf", "-std=gnu89",
		"-nostdinc", fmt.Sprintf("-D__NR_CPUS__=%d", common.GetNumPossibleCPUs(log)),
		"-Wall", "-Wextra", "-Werror", "-Wshadow",
		"-Wno-address-of-packed-member",
		"-Wno-unknown-warning-option",
		"-Wno-gnu-variable-sized-type-not-at-end",
		"-Wdeclaration-after-statement",
		"-Wimplicit-int-conversion",
		"-Wenum-conversion"}

	// testIncludes allows the unit tests to inject additional include
	// paths into the compile command at test time. It is usually nil.
	testIncludes []string

	debugProgs = []*progInfo{
		{
			Source:     endpointProg,
			Output:     endpointObjDebug,
			OutputType: outputObject,
		},
		{
			Source:     endpointProg,
			Output:     endpointAsm,
			OutputType: outputAssembly,
		},
		{
			Source:     endpointProg,
			Output:     endpointProg,
			OutputType: outputSource,
		},
	}
	debugHostProgs = []*progInfo{
		{
			Source:     hostEndpointProg,
			Output:     hostEndpointObjDebug,
			OutputType: outputObject,
		},
		{
			Source:     hostEndpointProg,
			Output:     hostEndpointAsm,
			OutputType: outputAssembly,
		},
		{
			Source:     hostEndpointProg,
			Output:     hostEndpointProg,
			OutputType: outputSource,
		},
	}
	epProg = &progInfo{
		Source:     endpointProg,
		Output:     endpointObj,
		OutputType: outputObject,
	}
	hostEpProg = &progInfo{
		Source:     hostEndpointProg,
		Output:     hostEndpointObj,
		OutputType: outputObject,
	}
	networkTcProg = &progInfo{
		Source:     networkProg,
		Output:     networkObj,
		OutputType: outputObject,
	}
)

// GetBPFCPU returns the BPF CPU for this host.
func GetBPFCPU() string {
	probeCPUOnce.Do(func() {
		if !option.Config.DryMode {
			// We can probe the availability of BPF instructions indirectly
			// based on what kernel helpers are available when both were
			// added in the same release.
			// We want to enable v3 only on kernels 5.10+ where we have
			// tested it and need it to work around complexity issues.
			if probes.HaveV3ISA() == nil {
				if probes.HaveProgramHelper(ebpf.SchedCLS, asm.FnRedirectNeigh) == nil {
					nameBPFCPU = "v3"
					return
				}
			}
			// We want to enable v2 on all kernels that support it, that is,
			// kernels 4.14+.
			if probes.HaveV2ISA() == nil {
				nameBPFCPU = "v2"
			}
		}
	})
	return nameBPFCPU
}

func pidFromProcess(proc *os.Process) string {
	result := "not-started"
	if proc != nil {
		result = fmt.Sprintf("%d", proc.Pid)
	}
	return result
}

// compile and optionally link a program.
//
// May output assembly or source code after prepocessing.
func compile(ctx context.Context, prog *progInfo, dir *directoryInfo) (string, error) {
	compileArgs := append(testIncludes,
		fmt.Sprintf("-I%s", path.Join(dir.Runtime, "globals")),
		fmt.Sprintf("-I%s", dir.State),
		fmt.Sprintf("-I%s", dir.Library),
		fmt.Sprintf("-I%s", path.Join(dir.Library, "include")),
	)

	switch prog.OutputType {
	case outputSource:
		compileArgs = append(compileArgs, "-E") // Preprocessor
	case outputAssembly:
		compileArgs = append(compileArgs, "-S")
		fallthrough
	case outputObject:
		compileArgs = append(compileArgs, "-g")
	}

	compileArgs = append(compileArgs, standardCFlags...)
	compileArgs = append(compileArgs, "-mcpu="+GetBPFCPU())
	compileArgs = append(compileArgs, prog.Options...)
	compileArgs = append(compileArgs,
		"-c", path.Join(dir.Library, prog.Source),
		"-o", "-", // Always output to stdout
	)

	log.WithFields(logrus.Fields{
		"target": compiler,
		"args":   compileArgs,
	}).Debug("Launching compiler")

	compileCmd, cancelCompile := exec.WithCancel(ctx, compiler, compileArgs...)
	defer cancelCompile()

	output, err := os.Create(path.Join(dir.Output, prog.Output))
	if err != nil {
		return "", err
	}
	defer output.Close()
	compileCmd.Stdout = output

	var compilerStderr bytes.Buffer
	compileCmd.Stderr = &compilerStderr

	err = compileCmd.Run()

	var maxRSS int64
	if usage, ok := compileCmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		maxRSS = usage.Maxrss
	}

	if err != nil {
		err = fmt.Errorf("Failed to compile %s: %w", prog.Output, err)

		if !errors.Is(err, context.Canceled) {
			log.WithFields(logrus.Fields{
				"compiler-pid": pidFromProcess(compileCmd.Process),
				"max-rss":      maxRSS,
			}).Error(err)
		}

		scanner := bufio.NewScanner(io.LimitReader(&compilerStderr, 1_000_000))
		for scanner.Scan() {
			log.Warn(scanner.Text())
		}

		return "", err
	}

	if maxRSS > 0 {
		log.WithFields(logrus.Fields{
			"compiler-pid": compileCmd.Process.Pid,
			"output":       output.Name(),
		}).Debugf("Compilation had peak RSS of %d bytes", maxRSS)
	}

	return output.Name(), nil
}

// compileDatapath invokes the compiler and linker to create all state files for
// the BPF datapath, with the primary target being the BPF ELF binary.
//
// It also creates the following output files:
// * Preprocessed C
// * Assembly
// * Object compiled with debug symbols
func compileDatapath(ctx context.Context, dirs *directoryInfo, isHost bool, logger *logrus.Entry) error {
	scopedLog := logger.WithField(logfields.Debug, true)

	versionCmd := exec.CommandContext(ctx, compiler, "--version")
	compilerVersion, err := versionCmd.CombinedOutput(scopedLog, true)
	if err != nil {
		return err
	}
	scopedLog.WithFields(logrus.Fields{
		compiler: string(compilerVersion),
	}).Debug("Compiling datapath")

	if option.Config.Debug {
		// Write out assembly and preprocessing files for debugging purposes
		progs := debugProgs
		if isHost {
			progs = debugHostProgs
		}
		for _, p := range progs {
			if _, err := compile(ctx, p, dirs); err != nil {
				// Only log an error here if the context was not canceled. This log message
				// should only represent failures with respect to compiling the program.
				if !errors.Is(err, context.Canceled) {
					scopedLog.WithField(logfields.Params, logfields.Repr(p)).WithError(err).Debug("JoinEP: Failed to compile")
				}
				return err
			}
		}
	}

	// Compile the new program
	prog := epProg
	if isHost {
		prog = hostEpProg
	}
	if _, err := compile(ctx, prog, dirs); err != nil {
		// Only log an error here if the context was not canceled. This log message
		// should only represent failures with respect to compiling the program.
		if !errors.Is(err, context.Canceled) {
			scopedLog.WithField(logfields.Params, logfields.Repr(prog)).WithError(err).Warn("JoinEP: Failed to compile")
		}
		return err
	}

	return nil
}

// CompileWithOptions compiles a BPF program generating an object file,
// using a set of provided compiler options.
func CompileWithOptions(ctx context.Context, src string, out string, opts []string) error {
	prog := progInfo{
		Source:     src,
		Options:    opts,
		Output:     out,
		OutputType: outputObject,
	}
	dirs := directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		Output:  option.Config.StateDir,
		State:   option.Config.StateDir,
	}
	_, err := compile(ctx, &prog, &dirs)
	return err
}

// Compile compiles a BPF program generating an object file.
func Compile(ctx context.Context, src string, out string) error {
	return CompileWithOptions(ctx, src, out, nil)
}

// compileTemplate compiles a BPF program generating a template object file.
func compileTemplate(ctx context.Context, out string, isHost bool) error {
	dirs := directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		Output:  out,
		State:   out,
	}
	return compileDatapath(ctx, &dirs, isHost, log)
}

// compileNetwork compiles a BPF program attached to network
func compileNetwork(ctx context.Context) error {
	dirs := directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		Output:  option.Config.StateDir,
		State:   option.Config.StateDir,
	}
	scopedLog := log.WithField(logfields.Debug, true)

	versionCmd := exec.CommandContext(ctx, compiler, "--version")
	compilerVersion, err := versionCmd.CombinedOutput(scopedLog, true)
	if err != nil {
		return err
	}
	scopedLog.WithFields(logrus.Fields{
		compiler: string(compilerVersion),
	}).Debug("Compiling network programs")

	// Write out assembly and preprocessing files for debugging purposes
	if _, err := compile(ctx, networkTcProg, &dirs); err != nil {
		scopedLog.WithField(logfields.Params, logfields.Repr(networkTcProg)).
			WithError(err).Warn("Failed to compile")
		return err
	}
	return nil
}

// compileOverlay compiles BPF programs in bpf_overlay.c.
func compileOverlay(ctx context.Context, opts []string) error {
	dirs := &directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		Output:  option.Config.StateDir,
		State:   option.Config.StateDir,
	}
	scopedLog := log.WithField(logfields.Debug, true)

	versionCmd := exec.CommandContext(ctx, compiler, "--version")
	compilerVersion, err := versionCmd.CombinedOutput(scopedLog, true)
	if err != nil {
		return err
	}
	scopedLog.WithFields(logrus.Fields{
		compiler: string(compilerVersion),
	}).Debug("Compiling overlay programs")

	prog := &progInfo{
		Source:     overlayProg,
		Output:     overlayObj,
		OutputType: outputObject,
		Options:    opts,
	}
	// Write out assembly and preprocessing files for debugging purposes
	if _, err := compile(ctx, prog, dirs); err != nil {
		scopedLog.WithField(logfields.Params, logfields.Repr(prog)).
			WithError(err).Warn("Failed to compile")
		return err
	}
	return nil
}
