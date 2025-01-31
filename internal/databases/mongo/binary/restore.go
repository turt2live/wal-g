package binary

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/internal/databases/mongo/common"
	"github.com/wal-g/wal-g/internal/databases/mongo/models"
)

type RestoreService struct {
	Context      context.Context
	LocalStorage *LocalStorage
	Uploader     internal.Uploader

	minimalConfigPath string
}

func CreateRestoreService(ctx context.Context, localStorage *LocalStorage, uploader internal.Uploader,
	minimalConfigPath string) (*RestoreService, error) {
	return &RestoreService{
		Context:           ctx,
		LocalStorage:      localStorage,
		Uploader:          uploader,
		minimalConfigPath: minimalConfigPath,
	}, nil
}

func (restoreService *RestoreService) DoRestore(
	backupName, restVersion string,
	rsConf RsConfig,
	shConf ShConfig,
	cfgConf MongoCfgConfig,
) error {
	disableHostResetup, err := internal.GetBoolSettingDefault(internal.MongoDBRestoreDisableHostResetup, false)
	if err != nil {
		return err
	}

	sentinel, err := common.DownloadSentinel(restoreService.Uploader.Folder(), backupName)
	if err != nil {
		return err
	}

	err = EnsureCompatibilityToRestoreMongodVersions(sentinel.MongoMeta.Version, restVersion)
	if err != nil {
		return err
	}

	err = restoreService.LocalStorage.EnsureMongodFsLockFileIsEmpty()
	if err != nil {
		return err
	}

	err = restoreService.LocalStorage.CleanupMongodDBPath()
	if err != nil {
		return err
	}

	tracelog.InfoLogger.Println("Download backup files to dbPath")
	err = restoreService.downloadFromTarArchives(backupName)
	if err != nil {
		return err
	}

	if !disableHostResetup {
		if err = restoreService.fixSystemData(rsConf, shConf, cfgConf); err != nil {
			return err
		}
		if err = restoreService.recoverFromOplogAsStandalone(sentinel); err != nil {
			return err
		}
	}

	return nil
}

func (restoreService *RestoreService) downloadFromTarArchives(backupName string) error {
	downloader := CreateConcurrentDownloader(restoreService.Uploader)
	return downloader.Download(backupName, restoreService.LocalStorage.MongodDBPath)
}

func (restoreService *RestoreService) fixSystemData(rsConfig RsConfig, shConfig ShConfig, mongocfgConfig MongoCfgConfig) error {
	mongodProcess, err := StartMongodWithDisableLogicalSessionCacheRefresh(restoreService.minimalConfigPath)
	if err != nil {
		return errors.Wrap(err, "unable to start mongod in special mode")
	}
	defer mongodProcess.Close()

	mongodService, err := CreateMongodService(
		restoreService.Context,
		"wal-g restore",
		mongodProcess.GetURI(),
		10*time.Minute,
	)
	if err != nil {
		return errors.Wrap(err, "unable to create mongod service")
	}

	err = mongodService.FixSystemDataAfterRestore(rsConfig, shConfig, mongocfgConfig)
	if err != nil {
		return err
	}

	err = mongodService.Shutdown()
	if err != nil {
		return err
	}

	return mongodProcess.Wait()
}

func (restoreService *RestoreService) recoverFromOplogAsStandalone(sentinel *models.Backup) error {
	mongodProcess, err := StartMongodWithRecoverFromOplogAsStandalone(restoreService.minimalConfigPath)
	if err != nil {
		return errors.Wrap(err, "unable to start mongod in special mode")
	}

	defer mongodProcess.Close()

	mongodService, err := CreateMongodService(
		restoreService.Context,
		"wal-g restore",
		mongodProcess.GetURI(),
		ComputeMongoStartTimeout(sentinel.UncompressedSize),
	)
	if err != nil {
		return errors.Wrap(err, "unable to create mongod service")
	}

	err = mongodService.Shutdown()
	if err != nil {
		return err
	}

	return mongodProcess.Wait()
}
