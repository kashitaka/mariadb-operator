package init

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	"github.com/mariadb-operator/mariadb-operator/pkg/environment"
	"github.com/mariadb-operator/mariadb-operator/pkg/galera/config"
	"github.com/mariadb-operator/mariadb-operator/pkg/galera/filemanager"
	"github.com/mariadb-operator/mariadb-operator/pkg/galera/state"
	"github.com/mariadb-operator/mariadb-operator/pkg/log"
	mariadbpod "github.com/mariadb-operator/mariadb-operator/pkg/pod"
	"github.com/mariadb-operator/mariadb-operator/pkg/statefulset"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	scheme    = runtime.NewScheme()
	logger    = ctrl.Log
	configDir string
	stateDir  string
)

const (
	wsrepSSTPidFile   = "wsrep_sst.pid"
	sstInProgressFile = "sst_in_progress"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))

	RootCmd.Flags().StringVar(&configDir, "config-dir", "/etc/mysql/mariadb.conf.d",
		"The directory that contains MariaDB configuration files")
	RootCmd.Flags().StringVar(&stateDir, "state-dir", "/var/lib/mysql", "The directory that contains MariaDB state files")
}

var RootCmd = &cobra.Command{
	Use:   "init",
	Short: "Init.",
	Long:  `Init container for Galera that co-operates with mariadb-operator.`,
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if err := log.SetupLoggerWithCommand(cmd); err != nil {
			fmt.Printf("error setting up logger: %v\n", err)
			os.Exit(1)
		}
		logger.Info("Starting init")

		ctx, cancel := newContext()
		defer cancel()

		env, err := environment.GetPodEnv(ctx)
		if err != nil {
			logger.Error(err, "Error getting environment variables")
			os.Exit(1)
		}
		fileManager, err := filemanager.NewFileManager(configDir, stateDir)
		if err != nil {
			logger.Error(err, "Error creating file manager")
			os.Exit(1)
		}
		k8sClient, err := getK8sClient()
		if err != nil {
			logger.Error(err, "Error getting Kubernetes client")
			os.Exit(1)
		}
		podIndex, err := statefulset.PodIndex(env.PodName)
		if err != nil {
			logger.Error(err, "error getting index from Pod", "pod", env.PodName)
			os.Exit(1)
		}

		key := types.NamespacedName{
			Name:      env.MariadbName,
			Namespace: env.PodNamespace,
		}
		var mdb mariadbv1alpha1.MariaDB
		if err := k8sClient.Get(ctx, key, &mdb); err != nil {
			logger.Error(err, "Error getting MariaDB")

			if err := updateGaleraConfig(fileManager, env); err != nil {
				logger.Error(err, "Error updating Galera config")
				os.Exit(1)
			}
			logger.Info("Updated Galera config")
			os.Exit(0)
		}

		if err := cleanupStateForVolumeSnapshot(fileManager, &mdb); err != nil {
			logger.Error(err, "error cleaning up state for VolumeSnapshot")
			os.Exit(1)
		}
		if err := configureGalera(fileManager, env, &mdb, logger); err != nil {
			logger.Error(err, "error configuring Galera")
			os.Exit(1)
		}
		if err := configureGaleraBootstrap(fileManager, &mdb, *podIndex); err != nil {
			logger.Error(err, "error configuring Galera bootstrap")
		}
		if err := waitForPreviousPod(ctx, fileManager, k8sClient, env, &mdb, *podIndex); err != nil {
			logger.Error(err, "error waiting for previous Pod")
			os.Exit(1)
		}
		if err := cleanupPreviousSST(fileManager); err != nil {
			logger.Error(err, "error cleaning up previous SST")
			os.Exit(1)
		}
		logger.Info("Init done")
	},
}

func newContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT}...,
	)
}

func getK8sClient() (client.Client, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error getting REST config: %v", err)
	}
	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes client: %v", err)
	}
	return k8sClient, nil
}

func configureGalera(fm *filemanager.FileManager, env *environment.PodEnvironment, mdb *mariadbv1alpha1.MariaDB, logger logr.Logger) error {
	logger.Info("Configuring Galera")

	configBytes, err := config.NewConfigFile(mdb, logger).Marshal(env)
	if err != nil {
		return fmt.Errorf("error getting Galera config: %v", err)
	}
	if err := fm.WriteConfigFile(config.ConfigFileName, configBytes); err != nil {
		return fmt.Errorf("error writing Galera config: %v", err)
	}
	return nil
}

func updateGaleraConfig(fm *filemanager.FileManager, env *environment.PodEnvironment) error {
	logger.Info("Updating existing Galera config")

	configBytes, err := fm.ReadConfigFile(config.ConfigFileName)
	if err != nil {
		return fmt.Errorf("error getting existing Galera config: %v", err)
	}

	updatedBytes, err := config.UpdateConfig(configBytes, env)
	if err != nil {
		return fmt.Errorf("error updating existing Galera config: %v", err)
	}
	if err := fm.WriteConfigFile(config.ConfigFileName, updatedBytes); err != nil {
		return fmt.Errorf("error writing existing Galera config: %v", err)
	}
	return nil
}

func configureGaleraBootstrap(fm *filemanager.FileManager, mdb *mariadbv1alpha1.MariaDB, podIndex int) error {
	hasGaleraState, err := hasGaleraState(fm)
	if err != nil {
		return fmt.Errorf("error checking Galera state: %v", err)
	}
	if mdb.HasGaleraConfiguredCondition() || hasGaleraState || podIndex != 0 {
		return nil
	}
	logger.Info("Configuring Galera bootstrap")

	if err := fm.WriteConfigFile(config.BootstrapFileName, config.BootstrapFile); err != nil {
		return fmt.Errorf("error configuring Galera bootstrap: %v", err)
	}
	return nil
}

func waitForPreviousPod(ctx context.Context, fm *filemanager.FileManager, k8sClient client.Client, env *environment.PodEnvironment,
	mdb *mariadbv1alpha1.MariaDB, podIndex int) error {
	hasGaleraState, err := hasGaleraState(fm)
	if err != nil {
		return fmt.Errorf("error checking Galera state: %v", err)
	}
	if mdb.HasGaleraConfiguredCondition() || hasGaleraState || podIndex == 0 {
		return nil
	}
	previousPodName, err := getPreviousPodName(mdb, podIndex)
	if err != nil {
		return fmt.Errorf("error getting previous Pod: %v", err)
	}

	logger.Info("Waiting for previous Pod to be ready", "pod", previousPodName)
	previousKey := types.NamespacedName{
		Name:      previousPodName,
		Namespace: env.PodNamespace,
	}
	if err := waitForPodReady(ctx, previousKey, k8sClient); err != nil {
		logger.Info("Waiting for previous Pod to be ready", "pod", previousPodName)
		return fmt.Errorf("error waiting for previous Pod '%s' to be ready: %v", previousPodName, err)
	}
	return nil
}

func cleanupStateForVolumeSnapshot(fm *filemanager.FileManager, mdb *mariadbv1alpha1.MariaDB) error {
	bootstrapFrom := ptr.Deref(mdb.Spec.BootstrapFrom, mariadbv1alpha1.BootstrapFrom{})
	if mdb.HasGaleraConfiguredCondition() || bootstrapFrom.VolumeSnapshotRef == nil {
		return nil
	}
	logger.Info("Cleaning up state for VolumeSnapshot")

	for _, file := range []string{state.GaleraStateFileName, state.GaleraPrimaryComponentStateFileName} {
		if err := cleanupStateFile(fm, file); err != nil {
			return err
		}
	}
	return nil
}

func cleanupPreviousSST(fm *filemanager.FileManager) error {
	for _, file := range []string{wsrepSSTPidFile, sstInProgressFile} {
		if err := cleanupStateFile(fm, file); err != nil {
			return err
		}
	}
	return nil
}

func getPreviousPodName(mdb *mariadbv1alpha1.MariaDB, podIndex int) (string, error) {
	if podIndex == 0 {
		return "", fmt.Errorf("Pod '%s' is the first Pod", statefulset.PodName(mdb.ObjectMeta, podIndex)) //nolint:staticcheck
	}
	previousPodIndex := podIndex - 1
	return statefulset.PodName(mdb.ObjectMeta, previousPodIndex), nil
}

func waitForPodReady(ctx context.Context, key types.NamespacedName, client client.Client) error {
	return wait.PollUntilContextCancel(ctx, 1*time.Second, true, func(context.Context) (bool, error) {
		var pod corev1.Pod
		if err := client.Get(ctx, key, &pod); err != nil {
			logger.V(1).Info("Error getting Pod", "err", err)
			return false, nil
		}
		if !mariadbpod.PodReady(&pod) {
			logger.V(1).Info("Pod not ready", "pod", pod.Name)
			return false, nil
		}
		logger.V(1).Info("Pod ready", "pod", pod.Name)
		return true, nil
	})
}

func hasGaleraState(fm *filemanager.FileManager) (bool, error) {
	stateExists, err := fm.StateFileExists(state.GaleraStateFileName)
	if err != nil {
		return false, fmt.Errorf("error checking Galera state file: %v", err)
	}
	primaryComponentExists, err := fm.StateFileExists(state.GaleraPrimaryComponentStateFileName)
	if err != nil {
		return false, fmt.Errorf("error checking Galera primary component file: %v", err)
	}
	return stateExists || primaryComponentExists, nil
}

func cleanupStateFile(fm *filemanager.FileManager, file string) error {
	exists, err := fm.StateFileExists(file)
	if err != nil {
		return fmt.Errorf("error checking if %s file exists: %v", file, err)
	}
	if exists {
		logger.Info("Deleting state file", "file", file)
		if err := fm.DeleteStateFile(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("error deleting %s file: %v", file, err)
		}
	}
	return nil
}
