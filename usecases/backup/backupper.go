//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2022 SeMI Technologies B.V. All rights reserved.
//
//  CONTACT: hello@semi.technology
//

package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/semi-technologies/weaviate/entities/backup"
	"github.com/semi-technologies/weaviate/entities/models"
	"github.com/semi-technologies/weaviate/usecases/config"
	"github.com/sirupsen/logrus"
)

type backupper struct {
	logger   logrus.FieldLogger
	sourcer  Sourcer
	backends BackupBackendProvider
	// shardCoordinationChan is sync and coordinate operations
	shardSyncChan
}

func newBackupper(logger logrus.FieldLogger, sourcer Sourcer, backends BackupBackendProvider,
) *backupper {
	return &backupper{
		logger:        logger,
		sourcer:       sourcer,
		backends:      backends,
		shardSyncChan: shardSyncChan{coordChan: make(chan interface{}, 5)},
	}
}

// Backup is called by the User
func (b *backupper) Backup(ctx context.Context,
	store objectStore, id string, classes []string,
) (*backup.CreateMeta, error) {
	// make sure there is no active backup
	req := Request{
		Method:  OpCreate,
		ID:      id,
		Classes: classes,
	}
	if _, err := b.backup(ctx, store, &req); err != nil {
		return nil, backup.NewErrUnprocessable(err)
	}

	return &backup.CreateMeta{
		Path:   store.HomeDir(id),
		Status: backup.Started,
	}, nil
}

// Status returns status of a backup
// If the backup is still active the status is immediately returned
// If not it fetches the metadata file to get the status
func (b *backupper) Status(ctx context.Context, backend, bakID string,
) (*models.BackupCreateStatusResponse, error) {
	// check if backup is still active
	st := b.lastOp.get()
	if st.ID == bakID {
		status := string(st.Status)
		return &models.BackupCreateStatusResponse{
			ID:      bakID,
			Path:    st.path,
			Status:  &status,
			Backend: backend,
		}, nil
	}

	// The backup might have been already created.
	store, err := b.objectStore(backend)
	if err != nil {
		err = fmt.Errorf("no backup provider %q, did you enable the right module?", backend)
		return nil, backup.NewErrUnprocessable(err)
	}

	meta, err := store.Meta(ctx, bakID)
	if err != nil {
		return nil, backup.NewErrNotFound(
			fmt.Errorf("backup status: get metafile %s/%s: %w", bakID, BackupFile, err))
	}

	status := string(meta.Status)

	return &models.BackupCreateStatusResponse{
		ID:      bakID,
		Path:    store.HomeDir(bakID),
		Status:  &status,
		Backend: backend,
	}, nil
}

func (b *backupper) objectStore(backend string) (objectStore, error) {
	caps, err := b.backends.BackupBackend(backend)
	if err != nil {
		return objectStore{}, err
	}
	return objectStore{caps}, nil
}

// Backup is called by the User
func (b *backupper) backup(ctx context.Context,
	store objectStore, req *Request,
) (CanCommitResponse, error) {
	id := req.ID
	expiration := req.Duration
	if expiration > _TimeoutShardCommit {
		expiration = _TimeoutShardCommit
	}
	ret := CanCommitResponse{
		Method:  OpCreate,
		ID:      req.ID,
		Timeout: expiration,
	}
	// make sure there is no active backup
	if prevID := b.lastOp.renew(id, time.Now(), store.HomeDir(id)); prevID != "" {
		return ret, fmt.Errorf("backup %s already in progress", prevID)
	}
	b.waitingForCoodinatorToCommit.Store(true) // is set to false by wait()

	go func() {
		defer b.lastOp.reset()
		if err := b.waitForCoordinator(expiration, id); err != nil {
			b.logger.WithField("action", "create_backup").
				Error(err)
			b.lastAsyncError = err
			return

		}
		provider := newUploader(b.sourcer, store, req.ID, b.lastOp.set)
		result := backup.BackupDescriptor{
			StartedAt:     time.Now().UTC(),
			ID:            id,
			Classes:       make([]backup.ClassDescriptor, 0, len(req.Classes)),
			Version:       Version,
			ServerVersion: config.ServerVersion,
		}
		if err := provider.all(context.Background(), req.Classes, &result); err != nil {
			b.logger.WithField("action", "create_backup").
				Error(err)
		}
		result.CompletedAt = time.Now().UTC()
	}()

	return ret, nil
}
