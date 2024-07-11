package main

import (
	"context"
	"flag"
	"github.com/loopholelabs/logging"
	"github.com/loopholelabs/sentry/pkg/rpc"
	"github.com/loopholelabs/sentry/pkg/vsock"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/loopholelabs/sentry/pkg/client"
)

func handle(beforeSuspend func(ctx context.Context) error, afterResume func(ctx context.Context) error) rpc.HandleFunc {
	return func(request *rpc.Request, response *rpc.Response) {
		switch request.Type {
		case ipc.BeforeSuspendType:
			_ = beforeSuspend(context.Background())
		case ipc.AfterResumeType:
			_ = afterResume(context.Background())
		}
	}
}

func main() {
	vsockPort := flag.Uint("vsock-port", 26, "VSock port")
	vsockTimeout := flag.Duration("vsock-timeout", time.Minute, "VSock dial timeout")
	_ = vsockTimeout
	shellCmd := flag.String("shell-cmd", "sh", "Shell to use to run the before suspend and after resume commands")
	beforeSuspendCmd := flag.String("before-suspend-cmd", "", "Command to run before the VM is suspended (leave empty to disable)")
	afterResumeCmd := flag.String("after-resume-cmd", "", "Command to run after the VM has been resumed (leave empty to disable)")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errs error
	defer func() {
		if errs != nil {
			panic(errs)
		}
	}()

	ctx, handlePanics, _, cancel, wait, _ := utils.GetPanicHandler(
		ctx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	go func() {
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt)

		<-done

		log.Println("Exiting gracefully")

		cancel()
	}()

	dialFunc, err := vsock.DialFunc(ipc.VSockCIDHost, uint32(*vsockPort))
	if err != nil {
		panic(err)
	}

	handleFunc := handle(
		func(ctx context.Context) error {
			log.Println("Running pre-suspend command")

			if strings.TrimSpace(*beforeSuspendCmd) != "" {
				cmd := exec.CommandContext(ctx, *shellCmd, "-c", *beforeSuspendCmd)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				if err := cmd.Run(); err != nil {
					return err
				}
			}

			return nil
		},
		func(ctx context.Context) error {
			log.Println("Running after-resume command")

			if strings.TrimSpace(*afterResumeCmd) != "" {
				cmd := exec.CommandContext(ctx, *shellCmd, "-c", *afterResumeCmd)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				if err := cmd.Run(); err != nil {
					return err
				}
			}

			return nil
		})

	sentry, err := client.New(&client.Options{
		Handle: handleFunc,
		Dial:   dialFunc,
		Logger: logging.NewNoopLogger(),
	})
	if err != nil {
		panic(err)
	}

	log.Println("waiting forever")
	select {}
	_ = sentry
}
