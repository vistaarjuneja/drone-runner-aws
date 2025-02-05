package harness

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/drone-runners/drone-runner-aws/internal/oshelp"

	"github.com/drone-runners/drone-runner-aws/command/config"
	"github.com/drone-runners/drone-runner-aws/engine/resource"
	"github.com/drone-runners/drone-runner-aws/internal/drivers"
	"github.com/drone-runners/drone-runner-aws/internal/lehelper"
	errors "github.com/drone-runners/drone-runner-aws/internal/types"
	"github.com/drone-runners/drone-runner-aws/store"
	"github.com/drone-runners/drone-runner-aws/types"
	"github.com/drone/runner-go/logger"
	"github.com/harness/lite-engine/api"
	lespec "github.com/harness/lite-engine/engine/spec"

	"github.com/sirupsen/logrus"
)

type SetupVMRequest struct {
	ID               string            `json:"id"` // stage runtime ID
	PoolID           string            `json:"pool_id"`
	FallbackPoolIDs  []string          `json:"fallback_pool_ids"`
	Tags             map[string]string `json:"tags"`
	CorrelationID    string            `json:"correlation_id"`
	LogKey           string            `json:"log_key"`
	api.SetupRequest `json:"setup_request"`
}

type SetupVMResponse struct {
	IPAddress  string `json:"ip_address"`
	InstanceID string `json:"instance_id"`
}

var (
	setupTimeout = 10 * time.Minute
)

func HandleSetup(ctx context.Context, r *SetupVMRequest, s store.StageOwnerStore, env *config.EnvConfig, poolManager *drivers.Manager) (*SetupVMResponse, error) {
	stageRuntimeID := r.ID
	if stageRuntimeID == "" {
		return nil, errors.NewBadRequestError("mandatory field 'id' in the request body is empty")
	}

	if r.PoolID == "" {
		return nil, errors.NewBadRequestError("mandatory field 'pool_id' in the request body is empty")
	}

	// Sets up logger to stream the logs in case log config is set
	log := logrus.New()
	var logr *logrus.Entry
	if r.SetupRequest.LogConfig.URL == "" {
		log.Out = os.Stdout
		logr = log.WithField("api", "dlite:setup").
			WithField("correlationID", r.CorrelationID)
	} else {
		wc := getStreamLogger(r.SetupRequest.LogConfig, r.LogKey, r.CorrelationID)
		defer func() {
			if err := wc.Close(); err != nil {
				logrus.WithError(err).Debugln("failed to close log stream")
			}
		}()

		log.Out = wc
		log.SetLevel(logrus.TraceLevel)
		logr = log.WithField("stage_runtime_id", stageRuntimeID)

		ctx = logger.WithContext(ctx, logger.Logrus(logr))
	}

	// append global volumes to the setup request.
	for _, pair := range env.Runner.Volumes {
		src, _, ro, err := resource.ParseVolume(pair)
		if err != nil {
			log.Warn(err)
			continue
		}
		vol := lespec.Volume{
			HostPath: &lespec.VolumeHostPath{
				ID:       fileID(src),
				Name:     fileID(src),
				Path:     src,
				ReadOnly: ro,
			},
		}
		r.Volumes = append(r.Volumes, &vol)
	}

	pools := []string{}
	pools = append(pools, r.PoolID)
	pools = append(pools, r.FallbackPoolIDs...)

	var poolErr error
	var err error
	var selectedPool string
	var instance *types.Instance
	foundPool := false

	for _, p := range pools {
		pool := fetchPool(r.SetupRequest.LogConfig.AccountID, p, env.Dlite.PoolMapByAccount)
		logr.WithField("pool_id", pool).Traceln("starting the setup process")

		if !poolManager.Exists(pool) {
			logr.WithField("pool_id", pool).Errorln("pool does not exist")
			continue
		}

		_, findErr := s.Find(ctx, stageRuntimeID)
		if findErr != nil {
			if cerr := s.Create(ctx, &types.StageOwner{StageID: stageRuntimeID, PoolName: pool}); cerr != nil {
				poolErr = fmt.Errorf("could not create stage owner entity: %w", cerr)
				logr.WithField("pool_id", pool).WithError(poolErr).Errorln("could not create stage owner entity")
				continue
			}
		}

		instance, err = poolManager.Provision(ctx, pool, env.Runner.Name, env)
		if err != nil {
			logr.WithError(err).WithField("pool_id", p).Errorln("failed to provision instance")
			poolErr = err
			if derr := s.Delete(ctx, stageRuntimeID); derr != nil {
				logr.WithField("pool_id", pool).WithError(derr).Errorln("could not remove stage ID mapping after provision failure")
			}
			continue
		}
		// Successfully provisioned an instance out of the listed pools
		foundPool = true
		selectedPool = pool
		break
	}

	if !foundPool {
		return nil, fmt.Errorf("could not provision a VM from the pool: %w", poolErr)
	}

	logr = logr.WithField("pool_id", selectedPool)

	logr = logr.
		WithField("ip", instance.Address).
		WithField("id", instance.ID).
		WithField("instance_name", instance.Name)

	logr.WithField("selected_pool", selectedPool).WithField("tried_pools", pools).Traceln("successfully provisioned VM in pool")

	// cleanUpFn is a function to terminate the instance if an error occurs later in the handleSetup function
	cleanUpFn := func(consoleLogs bool) {
		if consoleLogs {
			out, logErr := poolManager.InstanceLogs(context.Background(), selectedPool, instance.ID)
			if logErr != nil {
				logr.WithError(logErr).Errorln("failed to fetch console output logs")
			} else {
				logrus.WithField("id", instance.ID).
					WithField("instance_name", instance.Name).Infof("serial console output: %s", out)
			}
		}
		errCleanUp := poolManager.Destroy(context.Background(), selectedPool, instance.ID)
		if errCleanUp != nil {
			logr.WithError(errCleanUp).Errorln("failed to delete failed instance client")
		}
	}

	if instance.IsHibernated {
		instance, err = poolManager.StartInstance(ctx, selectedPool, instance.ID)
		if err != nil {
			go cleanUpFn(false)
			return nil, fmt.Errorf("failed to start the instance up")
		}
	}

	instance.Stage = stageRuntimeID
	instance.Updated = time.Now().Unix()
	err = poolManager.Update(ctx, instance)
	if err != nil {
		go cleanUpFn(false)
		return nil, fmt.Errorf("failed to tag: %w", err)
	}

	err = poolManager.SetInstanceTags(ctx, selectedPool, instance, r.Tags)
	if err != nil {
		go cleanUpFn(false)
		return nil, fmt.Errorf("failed to add tags to the instance: %w", err)
	}

	client, err := lehelper.GetClient(instance, env.Runner.Name, instance.Port, env.LiteEngine.EnableMock, env.LiteEngine.MockStepTimeoutSecs)
	if err != nil {
		go cleanUpFn(false)
		return nil, fmt.Errorf("failed to create LE client: %w", err)
	}

	// try the healthcheck api on the lite-engine until it responds ok
	logr.Traceln("running healthcheck and waiting for an ok response")
	if _, err = client.RetryHealth(ctx, setupTimeout); err != nil {
		go cleanUpFn(true)
		return nil, fmt.Errorf("failed to call lite-engine retry health: %w", err)
	}

	logr.Traceln("retry health check complete")

	// Currently m1 architecture does not enable nested virtualisation, so we disable docker.
	if instance.Platform.OS == oshelp.OSMac {
		b := false
		r.SetupRequest.MountDockerSocket = &b
	}

	setupResponse, err := client.Setup(ctx, &r.SetupRequest)
	if err != nil {
		go cleanUpFn(true)
		return nil, fmt.Errorf("failed to call setup lite-engine: %w", err)
	}

	logr.WithField("response", fmt.Sprintf("%+v", setupResponse)).Traceln("VM setup is complete")

	return &SetupVMResponse{InstanceID: instance.ID, IPAddress: instance.Address}, nil
}
