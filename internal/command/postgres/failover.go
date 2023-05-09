package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/mattn/go-colorable"
	"github.com/spf13/cobra"
	"github.com/superfly/flyctl/agent"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/flypg"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/command/apps"
	"github.com/superfly/flyctl/internal/command/ssh"
	"github.com/superfly/flyctl/internal/flag"
	mach "github.com/superfly/flyctl/internal/machine"
	"github.com/superfly/flyctl/internal/watch"
	"github.com/superfly/flyctl/iostreams"
)

func newFailover() *cobra.Command {
	const (
		short = "Failover to a new primary"
		long  = short + "\n"
		usage = "failover"
	)

	cmd := command.New(usage, short, long, runFailover,
		command.RequireSession,
		command.RequireAppName,
	)

	flag.Add(
		cmd,
		flag.App(),
		flag.AppConfig(),
	)

	return cmd
}

func runFailover(ctx context.Context) (err error) {
	var (
		MinPostgresHaVersion         = "0.0.20"
		MinPostgresFlexVersion       = "0.0.3"
		MinPostgresStandaloneVersion = "0.0.7"

		io      = iostreams.FromContext(ctx)
		client  = client.FromContext(ctx).API()
		appName = appconfig.NameFromContext(ctx)
	)

	app, err := client.GetAppCompact(ctx, appName)
	if err != nil {
		return fmt.Errorf("get app: %w", err)
	}

	if !app.IsPostgresApp() {
		return fmt.Errorf("app %s is not a Postgres app", app.Name)
	}

	if app.PlatformVersion != "machines" {
		return fmt.Errorf("failover is only supported for machines apps")
	}

	ctx, err = apps.BuildContext(ctx, app)
	if err != nil {
		return err
	}

	machines, releaseFunc, err := mach.AcquireAllLeases(ctx)
	defer releaseFunc(ctx, machines)
	if err != nil {
		return fmt.Errorf("machines could not be retrieved %w", err)
	}

	if err := hasRequiredVersionOnMachines(machines, MinPostgresHaVersion, MinPostgresFlexVersion, MinPostgresStandaloneVersion); err != nil {
		return err
	}

	// You can not failerover for single node postgres
	if len(machines) <= 1 {
		return fmt.Errorf("failover is not available for standalone postgres")
	}

	leader, err := pickLeader(ctx, machines)
	if err != nil {
		return err
	}

	if IsFlex(leader) {
		if failover_err := flexFailover(ctx, machines, app); failover_err != nil {
			if err := handleFlexFailoverFail(ctx, leader); err != nil {
				fmt.Fprintf(io.ErrOut, "Failed to handle failover failure, please manually configure PG cluster primary")
			}
			return fmt.Errorf("Failed to run migration command: %s", failover_err)
		}
	}

	flapsClient := flaps.FromContext(ctx)

	dialer := agent.DialerFromContext(ctx)

	pgclient := flypg.NewFromInstance(leader.PrivateIP, dialer)
	fmt.Fprintf(io.Out, "Performing a failover\n")
	if err := pgclient.Failover(ctx); err != nil {
		return fmt.Errorf("failed to trigger failover %w", err)
	}

	// Wait until the leader lost its role
	if err := retry.Do(
		func() error {
			var err error
			leader, err = flapsClient.Get(ctx, leader.ID)
			if err != nil {
				return err
			} else if machineRole(leader) == "leader" {
				return fmt.Errorf("%s hasn't lost its leader role", leader.ID)
			}
			return nil
		},
		retry.Context(ctx), retry.Attempts(30), retry.Delay(time.Second), retry.DelayType(retry.FixedDelay),
	); err != nil {
		return err
	}

	// wait for health checks to pass
	if err := watch.MachinesChecks(ctx, machines); err != nil {
		return fmt.Errorf("failed to wait for health checks to pass: %w", err)
	}

	fmt.Fprintf(io.Out, "Failover complete\n")
	return
}

func flexFailover(ctx context.Context, machines []*api.Machine, app *api.AppCompact) error {
	io := iostreams.FromContext(ctx)

	leader, err := pickLeader(ctx, machines)
	if err != nil {
		return err
	}
	flapsClient := flaps.FromContext(ctx)

	fmt.Fprintf(io.Out, "Performing a failover\n")

	primary_region := ""
	if len(machines) > 0 {
		for _, machine := range machines {
			if region, ok := machine.Config.Env["PRIMARY_REGION"]; ok {
				primary_region = region
			}
		}
	}
	if primary_region == "" {
		return fmt.Errorf("Could not find primary region for app")
	}

	newLeader, err := pickNewLeader(ctx, machines, primary_region)
	if err != nil {
		return err
	}

	// Stop the leader
	fmt.Println("Stopping current leader... ", leader.ID)
	machineStopInput := api.StopMachineInput{
		ID:     leader.ID,
		Signal: "SIGINT",
	}
	err = flapsClient.Stop(ctx, machineStopInput, leader.LeaseNonce)
	if err != nil {
		return fmt.Errorf("could not stop pg leader %s: %w", leader.ID, err)
	}

	fmt.Println("Starting new leader")
	_, err = flapsClient.Start(ctx, newLeader.ID, newLeader.LeaseNonce)
	if err != nil {
		return err
	}

	fmt.Println("Promoting new leader... ", newLeader.ID)
	err = ssh.SSHConnect(&ssh.SSHParams{
		Ctx:      ctx,
		Org:      app.Organization,
		App:      app.Name,
		Username: "postgres",
		Dialer:   agent.DialerFromContext(ctx),
		Cmd:      "repmgr standby promote --siblings-follow -f /data/repmgr.conf",
		Stdout:   ioutils.NewWriteCloserWrapper(colorable.NewColorableStdout(), func() error { return nil }),
		Stderr:   ioutils.NewWriteCloserWrapper(colorable.NewColorableStderr(), func() error { return nil }),
		Stdin:    os.Stdin,
	}, newLeader.PrivateIP)
	if err != nil {
		return err

	}

	// Restart the old leader
	fmt.Fprintf(io.Out, "Restarting old leader... %s\n", leader.ID)
	mach, err := flapsClient.Start(ctx, leader.ID, leader.LeaseNonce)
	if err != nil {
		return err
	}
	if mach.Status == "error" {
		return fmt.Errorf("old leader %s could not be started: %s", leader.ID, mach.Message)
	}

	fmt.Printf("Waiting for leadership to swap to %s...\n", newLeader.ID)
	if err := retry.Do(
		func() error {
			leader, err = flapsClient.Get(ctx, newLeader.ID)
			if err != nil {
				return err
			}

			if isLeader(leader) {
				return nil
			} else {
				return fmt.Errorf("Machine %s never became the leader", newLeader.ID)
			}

		},
		retry.Context(ctx), retry.Attempts(60), retry.Delay(time.Second), retry.DelayType(retry.FixedDelay),
	); err != nil {
		return err
	}

	// wait for health checks to pass
	if err := watch.MachinesChecks(ctx, machines); err != nil {
		return fmt.Errorf("failed to wait for health checks to pass: %w", err)
	}

	fmt.Fprintf(io.Out, "Failover complete\n")
	return nil
}

func handleFlexFailoverFail(ctx context.Context, leader *api.Machine) (err error) {
	io := iostreams.FromContext(ctx)
	flapsClient := flaps.FromContext(ctx)

	fmt.Fprintln(io.ErrOut, "Error promoting new leader, restarting existing leader")
	fmt.Println("Waiting for old leader to finish stopping")
	if err := retry.Do(
		func() error {
			leader, err = flapsClient.Get(ctx, leader.ID)
			if err != nil {
				return err
			}

			if leader.State == "stopped" {
				return nil
			} else if leader.State == "stopping" {
				return fmt.Errorf("Old leader hasn't finished stopping")
			} else {
				return fmt.Errorf("Old leader is in an unexpected state: %s", leader.State)
			}

		},
		retry.Context(ctx), retry.Attempts(60), retry.Delay(time.Second), retry.DelayType(retry.FixedDelay),
	); err != nil {
		return err
	}

	fmt.Println("Clearing existing machine lease...")

	// Clear the existing lease on this machine
	lease, err := flapsClient.FindLease(ctx, leader.ID)
	if err != nil {
		if !strings.Contains(err.Error(), " lease not found") {
			return err
		}
	}
	if err := flapsClient.ReleaseLease(ctx, leader.ID, lease.Data.Nonce); err != nil {
		return err
	}

	fmt.Println("Trying to start old leader")
	// Start the machine again
	leader, err = flapsClient.Get(ctx, leader.ID)
	if err != nil {
		return err
	}

	mach, err := flapsClient.Start(ctx, leader.ID, leader.LeaseNonce)
	if err != nil {
		return err
	}
	if mach.Status == "error" {
		return fmt.Errorf("old leader %s could not be started: %s", leader.ID, mach.Message)
	}

	return nil
}
