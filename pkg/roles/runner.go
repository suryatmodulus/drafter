package roles

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/loopholelabs/logging"
	"github.com/loopholelabs/sentry/pkg/rpc"
	"github.com/loopholelabs/sentry/pkg/server"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lithammer/shortuuid/v4"
	"github.com/loopholelabs/drafter/internal/firecracker"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/utils"
)

const (
	VSockName = "vsock.sock"
)

type HypervisorConfiguration struct {
	FirecrackerBin string
	JailerBin      string

	ChrootBaseDir string

	UID int
	GID int

	NetNS         string
	NumaNode      int
	CgroupVersion int

	EnableOutput bool
	EnableInput  bool
}

type NetworkConfiguration struct {
	Interface string
	MAC       string
}

type VMConfiguration struct {
	CPUCount    int
	MemorySize  int
	CPUTemplate string

	BootArgs string
}

type PackageConfiguration struct {
	AgentVSockPort uint32 `json:"agentVSockPort"`
}

type SnapshotLoadConfiguration struct {
	ExperimentalMapPrivate bool

	ExperimentalMapPrivateStateOutput  string
	ExperimentalMapPrivateMemoryOutput string
}

type Runner struct {
	VMPath string

	Wait  func() error
	Close func() error

	Resume func(
		ctx context.Context,

		resumeTimeout time.Duration,
		rescueTimeout time.Duration,
		agentVSockPort uint32,

		snapshotLoadConfiguration SnapshotLoadConfiguration,
	) (
		resumedRunner *ResumedRunner,

		errs error,
	)
}

type ResumedRunner struct {
	Wait  func() error
	Close func() error

	Msync                      func(ctx context.Context) error
	SuspendAndCloseAgentServer func(ctx context.Context, suspendTimeout time.Duration) error
}

var (
	ErrCouldNotCreateChrootBaseDirectory = errors.New("could not create chroot base directory")
	ErrCouldNotStartFirecrackerServer    = errors.New("could not start firecracker server")
	ErrCouldNotWaitForFirecracker        = errors.New("could not wait for firecracker")
	ErrCouldNotCloseServer               = errors.New("could not close server")
	ErrCouldNotRemoveVMDir               = errors.New("could not remove VM directory")
	ErrCouldNotStartAgentServer          = errors.New("could not start agent server")
	ErrCouldNotCloseAgent                = errors.New("could not close agent")
	ErrCouldNotChownVSockPath            = errors.New("could not change ownership of vsock path")
	ErrCouldNotResumeSnapshot            = errors.New("could not resume snapshot")
	ErrCouldNotAcceptAgent               = errors.New("could not accept agent")
	ErrCouldNotWaitForAcceptingAgent     = errors.New("could not wait for accepting agent")
	ErrCouldNotCloseAcceptingAgent       = errors.New("could not close accepting agent")
	ErrCouldNotCallAfterResumeRPC        = errors.New("could not call AfterResume RPC")
	ErrCouldNotCallBeforeSuspendRPC      = errors.New("could not call BeforeSuspend RPC")
	ErrCouldNotCreateSnapshot            = errors.New("could not create snapshot")
	ErrCouldNotCreateRecoverySnapshot    = errors.New("could not create recovery snapshot")
)

func StartRunner(
	hypervisorCtx context.Context,
	rescueCtx context.Context,

	hypervisorConfiguration HypervisorConfiguration,

	stateName string,
	memoryName string,
) (
	runner *Runner,

	errs error,
) {
	runner = &Runner{}

	_, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		hypervisorCtx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	if err := os.MkdirAll(hypervisorConfiguration.ChrootBaseDir, os.ModePerm); err != nil {
		panic(errors.Join(ErrCouldNotCreateChrootBaseDirectory, err))
	}

	var ongoingResumeWg sync.WaitGroup

	firecrackerCtx, cancelFirecrackerCtx := context.WithCancel(rescueCtx) // We use `rescueContext` here since this simply intercepts `hypervisorCtx`
	// and then waits for `rescueCtx` or the rescue operation to complete
	go func() {
		<-hypervisorCtx.Done() // We use hypervisorCtx, not internalCtx here since this resource outlives the function call

		ongoingResumeWg.Wait()

		cancelFirecrackerCtx()
	}()

	fcServer, err := firecracker.StartFirecrackerServer(
		firecrackerCtx, // We use firecrackerCtx (which depends on hypervisorCtx, not internalCtx) here since this resource outlives the function call

		hypervisorConfiguration.FirecrackerBin,
		hypervisorConfiguration.JailerBin,

		hypervisorConfiguration.ChrootBaseDir,

		hypervisorConfiguration.UID,
		hypervisorConfiguration.GID,

		hypervisorConfiguration.NetNS,
		hypervisorConfiguration.NumaNode,
		hypervisorConfiguration.CgroupVersion,

		hypervisorConfiguration.EnableOutput,
		hypervisorConfiguration.EnableInput,
	)
	if err != nil {
		panic(errors.Join(ErrCouldNotStartFirecrackerServer, err))
	}

	runner.VMPath = fcServer.VMPath

	// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
	// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
	handleGoroutinePanics(false, func() {
		if err := fcServer.Wait(); err != nil {
			panic(errors.Join(ErrCouldNotWaitForFirecracker, err))
		}
	})

	runner.Wait = fcServer.Wait
	runner.Close = func() error {
		if err := fcServer.Close(); err != nil {
			return errors.Join(ErrCouldNotCloseServer, err)
		}

		if err := runner.Wait(); err != nil {
			return errors.Join(ErrCouldNotWaitForFirecracker, err)
		}

		if err := os.RemoveAll(filepath.Dir(runner.VMPath)); err != nil {
			return errors.Join(ErrCouldNotRemoveVMDir, err)
		}

		return nil
	}

	firecrackerClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(runner.VMPath, firecracker.FirecrackerSocketName))
			},
		},
	}

	runner.Resume = func(
		ctx context.Context,

		resumeTimeout,
		rescueTimeout time.Duration,
		agentVSockPort uint32,

		snapshotLoadConfiguration SnapshotLoadConfiguration,
	) (
		resumedRunner *ResumedRunner,

		errs error,
	) {
		resumedRunner = &ResumedRunner{}

		ongoingResumeWg.Add(1)
		defer ongoingResumeWg.Done()

		var (
			sentry                  *server.Server
			suspendOnPanicWithError = false
		)

		createSnapshot := func(ctx context.Context) error {
			var (
				stateCopyName  = shortuuid.New()
				memoryCopyName = shortuuid.New()
			)
			if snapshotLoadConfiguration.ExperimentalMapPrivate {
				if err := firecracker.CreateSnapshot(
					ctx,

					firecrackerClient,

					// We need to write the state and memory to a separate file since we can't truncate an `mmap`ed file
					stateCopyName,
					memoryCopyName,

					firecracker.SnapshotTypeFull,
				); err != nil {
					return errors.Join(ErrCouldNotCreateSnapshot, err)
				}
			} else {
				if err := firecracker.CreateSnapshot(
					ctx,

					firecrackerClient,

					stateName,
					"",

					firecracker.SnapshotTypeMsyncAndState,
				); err != nil {
					return errors.Join(ErrCouldNotCreateSnapshot, err)
				}
			}

			if snapshotLoadConfiguration.ExperimentalMapPrivate {
				if err := fcServer.Close(); err != nil {
					return errors.Join(ErrCouldNotCloseServer, err)
				}

				if err := runner.Wait(); err != nil {
					return errors.Join(ErrCouldNotWaitForFirecracker, err)
				}

				for _, device := range [][3]string{
					{stateName, stateCopyName, snapshotLoadConfiguration.ExperimentalMapPrivateStateOutput},
					{memoryName, memoryCopyName, snapshotLoadConfiguration.ExperimentalMapPrivateMemoryOutput},
				} {
					inputFile, err := os.Open(filepath.Join(fcServer.VMPath, device[1]))
					if err != nil {
						return errors.Join(ErrCouldNotOpenInputFile, err)
					}
					defer inputFile.Close()

					var (
						outputPath = device[2]
						addPadding = true
					)
					if outputPath == "" {
						outputPath = filepath.Join(fcServer.VMPath, device[0])
						addPadding = false
					}

					if err := os.MkdirAll(filepath.Dir(outputPath), os.ModePerm); err != nil {
						panic(errors.Join(ErrCouldNotCreateOutputDir, err))
					}

					outputFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
					if err != nil {
						return errors.Join(ErrCouldNotOpenOutputFile, err)
					}
					defer outputFile.Close()

					deviceSize, err := io.Copy(outputFile, inputFile)
					if err != nil {
						return errors.Join(ErrCouldNotCopyFile, err)
					}

					// We need to add a padding like the snapshotter if we're writing to a file instead of a block device
					if addPadding {
						if paddingLength := utils.GetBlockDevicePadding(deviceSize); paddingLength > 0 {
							if _, err := outputFile.Write(make([]byte, paddingLength)); err != nil {
								return errors.Join(ErrCouldNotWritePadding, err)
							}
						}
					}
				}
			}

			return nil
		}

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			ctx,
			&errs,
			utils.GetPanicHandlerHooks{
				OnAfterRecover: func() {
					if suspendOnPanicWithError {
						suspendCtx, cancelSuspendCtx := context.WithTimeout(rescueCtx, rescueTimeout)
						defer cancelSuspendCtx()
						if sentry != nil {
							_ = sentry.Close()
						}
						// If a resume failed, flush the snapshot so that we can re-try
						if err := createSnapshot(suspendCtx); err != nil {
							errs = errors.Join(errs, ErrCouldNotCreateRecoverySnapshot, err)
						}
					}
				},
			},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
		// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
		handleGoroutinePanics(false, func() {
			if err := fcServer.Wait(); err != nil {
				panic(errors.Join(ErrCouldNotWaitForFirecracker, err))
			}
		})

		sentryPath := fmt.Sprintf("%s_%d", filepath.Join(fcServer.VMPath, VSockName), agentVSockPort)
		sentry, err = server.New(&server.Options{
			UnixPath: sentryPath,
			MaxConn:  32,
			Logger:   logging.NewNoopLogger(),
		})
		if err != nil {
			panic(errors.Join(ErrCouldNotStartAgentServer, err))
		}

		resumedRunner.Close = func() error {
			_ = sentry.Close()
			return nil
		}

		if err := os.Chown(sentryPath, hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
			panic(errors.Join(ErrCouldNotChownVSockPath, err))
		}

		{
			resumeSnapshotAndAcceptCtx, cancelResumeSnapshotAndAcceptCtx := context.WithTimeout(internalCtx, resumeTimeout)
			defer cancelResumeSnapshotAndAcceptCtx()

			if err := firecracker.ResumeSnapshot(
				resumeSnapshotAndAcceptCtx,

				firecrackerClient,

				stateName,
				memoryName,

				!snapshotLoadConfiguration.ExperimentalMapPrivate,
			); err != nil {
				panic(errors.Join(ErrCouldNotResumeSnapshot, err))
			}

			suspendOnPanicWithError = true
		}

		resumedRunner.Close = func() error {
			_ = sentry.Close()
			if err := resumedRunner.Wait(); err != nil {
				return errors.Join(ErrCouldNotWaitForAcceptingAgent, err)
			}
			return nil
		}

		{
			afterResumeCtx, cancelAfterResumeCtx := context.WithTimeout(internalCtx, resumeTimeout)
			defer cancelAfterResumeCtx()

			request := rpc.Request{
				UUID: uuid.New(),
				Type: ipc.AfterResumeType,
				Data: nil,
			}
			var response rpc.Response
			err = sentry.Do(afterResumeCtx, &request, &response)
			if err != nil {
				panic(errors.Join(ErrCouldNotCallAfterResumeRPC, err))
			}
		}

		resumedRunner.Msync = func(ctx context.Context) error {
			if !snapshotLoadConfiguration.ExperimentalMapPrivate {
				if err := firecracker.CreateSnapshot(
					ctx,

					firecrackerClient,

					stateName,
					"",

					firecracker.SnapshotTypeMsync,
				); err != nil {
					return errors.Join(ErrCouldNotCreateSnapshot, err)
				}
			}

			return nil
		}

		resumedRunner.SuspendAndCloseAgentServer = func(ctx context.Context, suspendTimeout time.Duration) error {
			suspendCtx, cancel := context.WithTimeout(ctx, suspendTimeout)
			defer cancel()

			request := rpc.Request{
				UUID: uuid.New(),
				Type: ipc.BeforeSuspendType,
				Data: nil,
			}
			var response rpc.Response

			err = sentry.Do(suspendCtx, &request, &response)
			if err != nil {
				panic(errors.Join(ErrCouldNotCallBeforeSuspendRPC, err))
			}
			_ = sentry.Close()

			if err := createSnapshot(suspendCtx); err != nil {
				return errors.Join(ErrCouldNotCreateSnapshot, err)
			}

			return nil
		}

		return
	}

	return
}
