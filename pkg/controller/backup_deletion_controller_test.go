/*
Copyright 2018, 2019 the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/sets"
	core "k8s.io/client-go/testing"

	v1 "github.com/heptio/velero/pkg/apis/velero/v1"
	pkgbackup "github.com/heptio/velero/pkg/backup"
	"github.com/heptio/velero/pkg/builder"
	"github.com/heptio/velero/pkg/generated/clientset/versioned/fake"
	informers "github.com/heptio/velero/pkg/generated/informers/externalversions"
	"github.com/heptio/velero/pkg/metrics"
	"github.com/heptio/velero/pkg/persistence"
	persistencemocks "github.com/heptio/velero/pkg/persistence/mocks"
	"github.com/heptio/velero/pkg/plugin/clientmgmt"
	pluginmocks "github.com/heptio/velero/pkg/plugin/mocks"
	velerotest "github.com/heptio/velero/pkg/util/test"
	"github.com/heptio/velero/pkg/volume"
)

func TestBackupDeletionControllerProcessQueueItem(t *testing.T) {
	client := fake.NewSimpleClientset()
	sharedInformers := informers.NewSharedInformerFactory(client, 0)

	controller := NewBackupDeletionController(
		velerotest.NewLogger(),
		sharedInformers.Velero().V1().DeleteBackupRequests(),
		client.VeleroV1(), // deleteBackupRequestClient
		client.VeleroV1(), // backupClient
		sharedInformers.Velero().V1().Restores(),
		client.VeleroV1(), // restoreClient
		NewBackupTracker(),
		nil, // restic repository manager
		sharedInformers.Velero().V1().PodVolumeBackups(),
		sharedInformers.Velero().V1().BackupStorageLocations(),
		sharedInformers.Velero().V1().VolumeSnapshotLocations(),
		nil, // new plugin manager func
		metrics.NewServerMetrics(),
	).(*backupDeletionController)

	// Error splitting key
	err := controller.processQueueItem("foo/bar/baz")
	assert.Error(t, err)

	// Can't find DeleteBackupRequest
	err = controller.processQueueItem("foo/bar")
	assert.NoError(t, err)

	// Already processed
	req := pkgbackup.NewDeleteBackupRequest("foo", "uid")
	req.Namespace = "foo"
	req.Name = "foo-abcde"
	req.Status.Phase = v1.DeleteBackupRequestPhaseProcessed

	err = controller.processQueueItem("foo/bar")
	assert.NoError(t, err)

	// Invoke processRequestFunc
	for _, phase := range []v1.DeleteBackupRequestPhase{"", v1.DeleteBackupRequestPhaseNew, v1.DeleteBackupRequestPhaseInProgress} {
		t.Run(fmt.Sprintf("phase=%s", phase), func(t *testing.T) {
			req.Status.Phase = phase
			sharedInformers.Velero().V1().DeleteBackupRequests().Informer().GetStore().Add(req)

			var errorToReturn error
			var actual *v1.DeleteBackupRequest
			var called bool
			controller.processRequestFunc = func(r *v1.DeleteBackupRequest) error {
				called = true
				actual = r
				return errorToReturn
			}

			// No error
			err = controller.processQueueItem("foo/foo-abcde")
			require.True(t, called, "processRequestFunc wasn't called")
			assert.Equal(t, err, errorToReturn)
			assert.Equal(t, req, actual)

			// Error
			errorToReturn = errors.New("bar")
			err = controller.processQueueItem("foo/foo-abcde")
			require.True(t, called, "processRequestFunc wasn't called")
			assert.Equal(t, err, errorToReturn)
		})
	}
}

type backupDeletionControllerTestData struct {
	client            *fake.Clientset
	sharedInformers   informers.SharedInformerFactory
	volumeSnapshotter *velerotest.FakeVolumeSnapshotter
	backupStore       *persistencemocks.BackupStore
	controller        *backupDeletionController
	req               *v1.DeleteBackupRequest
}

func setupBackupDeletionControllerTest(objects ...runtime.Object) *backupDeletionControllerTestData {
	req := pkgbackup.NewDeleteBackupRequest("foo", "uid")
	req.Namespace = "velero"
	req.Name = "foo-abcde"

	var (
		client            = fake.NewSimpleClientset(append(objects, req)...)
		sharedInformers   = informers.NewSharedInformerFactory(client, 0)
		volumeSnapshotter = &velerotest.FakeVolumeSnapshotter{SnapshotsTaken: sets.NewString()}
		pluginManager     = &pluginmocks.Manager{}
		backupStore       = &persistencemocks.BackupStore{}
	)

	data := &backupDeletionControllerTestData{
		client:            client,
		sharedInformers:   sharedInformers,
		volumeSnapshotter: volumeSnapshotter,
		backupStore:       backupStore,
		controller: NewBackupDeletionController(
			velerotest.NewLogger(),
			sharedInformers.Velero().V1().DeleteBackupRequests(),
			client.VeleroV1(), // deleteBackupRequestClient
			client.VeleroV1(), // backupClient
			sharedInformers.Velero().V1().Restores(),
			client.VeleroV1(), // restoreClient
			NewBackupTracker(),
			nil, // restic repository manager
			sharedInformers.Velero().V1().PodVolumeBackups(),
			sharedInformers.Velero().V1().BackupStorageLocations(),
			sharedInformers.Velero().V1().VolumeSnapshotLocations(),
			func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager },
			metrics.NewServerMetrics(),
		).(*backupDeletionController),

		req: req,
	}

	data.controller.newBackupStore = func(*v1.BackupStorageLocation, persistence.ObjectStoreGetter, logrus.FieldLogger) (persistence.BackupStore, error) {
		return backupStore, nil
	}

	pluginManager.On("CleanupClients").Return(nil)

	return data
}

func TestBackupDeletionControllerProcessRequest(t *testing.T) {
	t.Run("missing spec.backupName", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()

		td.req.Spec.BackupName = ""

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"errors":["spec.backupName is required"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("existing deletion requests for the backup are deleted", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()

		// add the backup to the tracker so the execution of processRequest doesn't progress
		// past checking for an in-progress backup. this makes validation easier.
		td.controller.backupTracker.Add(td.req.Namespace, td.req.Spec.BackupName)

		require.NoError(t, td.sharedInformers.Velero().V1().DeleteBackupRequests().Informer().GetStore().Add(td.req))

		existing := &v1.DeleteBackupRequest{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: td.req.Namespace,
				Name:      "bar",
				Labels: map[string]string{
					v1.BackupNameLabel: td.req.Spec.BackupName,
				},
			},
			Spec: v1.DeleteBackupRequestSpec{
				BackupName: td.req.Spec.BackupName,
			},
		}
		require.NoError(t, td.sharedInformers.Velero().V1().DeleteBackupRequests().Informer().GetStore().Add(existing))
		_, err := td.client.VeleroV1().DeleteBackupRequests(td.req.Namespace).Create(existing)
		require.NoError(t, err)

		require.NoError(t, td.sharedInformers.Velero().V1().DeleteBackupRequests().Informer().GetStore().Add(
			&v1.DeleteBackupRequest{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: td.req.Namespace,
					Name:      "bar-2",
					Labels: map[string]string{
						v1.BackupNameLabel: "some-other-backup",
					},
				},
				Spec: v1.DeleteBackupRequestSpec{
					BackupName: "some-other-backup",
				},
			},
		))

		assert.NoError(t, td.controller.processRequest(td.req))

		expectedDeleteAction := core.NewDeleteAction(
			v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
			td.req.Namespace,
			"bar",
		)

		// first action is the Create of an existing DBR for the backup as part of test data setup
		// second action is the Delete of the existing DBR, which we're validating
		// third action is the Patch of the DBR to set it to processed with an error
		require.Len(t, td.client.Actions(), 3)
		assert.Equal(t, expectedDeleteAction, td.client.Actions()[1])
	})

	t.Run("deleting an in progress backup isn't allowed", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()

		td.controller.backupTracker.Add(td.req.Namespace, td.req.Spec.BackupName)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"errors":["backup is still in progress"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("patching to InProgress fails", func(t *testing.T) {
		backup := builder.ForBackup(v1.DefaultNamespace, "foo").StorageLocation("default").Result()
		location := builder.ForBackupStorageLocation("velero", "default").Result()

		td := setupBackupDeletionControllerTest(backup)

		td.sharedInformers.Velero().V1().BackupStorageLocations().Informer().GetStore().Add(location)

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("bad")
		})

		err := td.controller.processRequest(td.req)
		assert.EqualError(t, err, "error patching DeleteBackupRequest: bad")

		expectedActions := []core.Action{
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				backup.Namespace,
				backup.Name,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"InProgress"}}`),
			),
		}
		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("patching backup to Deleting fails", func(t *testing.T) {
		backup := builder.ForBackup(v1.DefaultNamespace, "foo").StorageLocation("default").Result()
		location := builder.ForBackupStorageLocation("velero", "default").Result()

		td := setupBackupDeletionControllerTest(backup)

		td.sharedInformers.Velero().V1().BackupStorageLocations().Informer().GetStore().Add(location)

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})
		td.client.PrependReactor("patch", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("bad")
		})

		err := td.controller.processRequest(td.req)
		assert.EqualError(t, err, "error patching Backup: bad")

		expectedActions := []core.Action{
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				backup.Namespace,
				backup.Name,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"InProgress"}}`),
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				backup.Namespace,
				backup.Name,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"Deleting"}}`),
			),
		}
		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("unable to find backup", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"errors":["backup not found"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("unable to find backup storage location", func(t *testing.T) {
		backup := builder.ForBackup(v1.DefaultNamespace, "foo").StorageLocation("default").Result()

		td := setupBackupDeletionControllerTest(backup)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"errors":["backup storage location default not found"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("backup storage location is in read-only mode", func(t *testing.T) {
		backup := builder.ForBackup(v1.DefaultNamespace, "foo").StorageLocation("default").Result()
		location := builder.ForBackupStorageLocation("velero", "default").AccessMode(v1.BackupStorageLocationAccessModeReadOnly).Result()

		td := setupBackupDeletionControllerTest(backup)

		td.sharedInformers.Velero().V1().BackupStorageLocations().Informer().GetStore().Add(location)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"errors":["cannot delete backup because backup storage location default is currently in read-only mode"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("full delete, no errors", func(t *testing.T) {
		backup := builder.ForBackup(v1.DefaultNamespace, "foo").Result()
		backup.UID = "uid"
		backup.Spec.StorageLocation = "primary"

		restore1 := builder.ForRestore("velero", "restore-1").Phase(v1.RestorePhaseCompleted).Backup("foo").Result()
		restore2 := builder.ForRestore("velero", "restore-2").Phase(v1.RestorePhaseCompleted).Backup("foo").Result()
		restore3 := builder.ForRestore("velero", "restore-3").Phase(v1.RestorePhaseCompleted).Backup("some-other-backup").Result()

		td := setupBackupDeletionControllerTest(backup, restore1, restore2, restore3)

		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore1)
		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore2)
		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore3)

		location := &v1.BackupStorageLocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: backup.Namespace,
				Name:      backup.Spec.StorageLocation,
			},
			Spec: v1.BackupStorageLocationSpec{
				Provider: "objStoreProvider",
				StorageType: v1.StorageType{
					ObjectStorage: &v1.ObjectStorageLocation{
						Bucket: "bucket",
					},
				},
			},
		}
		require.NoError(t, td.sharedInformers.Velero().V1().BackupStorageLocations().Informer().GetStore().Add(location))

		snapshotLocation := &v1.VolumeSnapshotLocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: backup.Namespace,
				Name:      "vsl-1",
			},
			Spec: v1.VolumeSnapshotLocationSpec{
				Provider: "provider-1",
			},
		}
		require.NoError(t, td.sharedInformers.Velero().V1().VolumeSnapshotLocations().Informer().GetStore().Add(snapshotLocation))

		// Clear out req labels to make sure the controller adds them and does not
		// panic when encountering a nil Labels map
		// (https://github.com/heptio/velero/issues/1546)
		td.req.Labels = nil

		td.client.PrependReactor("get", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})
		td.volumeSnapshotter.SnapshotsTaken.Insert("snap-1")

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})

		td.client.PrependReactor("patch", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})

		snapshots := []*volume.Snapshot{
			{
				Spec: volume.SnapshotSpec{
					Location: "vsl-1",
				},
				Status: volume.SnapshotStatus{
					ProviderSnapshotID: "snap-1",
				},
			},
		}

		pluginManager := &pluginmocks.Manager{}
		pluginManager.On("GetVolumeSnapshotter", "provider-1").Return(td.volumeSnapshotter, nil)
		pluginManager.On("CleanupClients")
		td.controller.newPluginManager = func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager }

		td.backupStore.On("GetBackupVolumeSnapshots", td.req.Spec.BackupName).Return(snapshots, nil)
		td.backupStore.On("DeleteBackup", td.req.Spec.BackupName).Return(nil)
		td.backupStore.On("DeleteRestore", "restore-1").Return(nil)
		td.backupStore.On("DeleteRestore", "restore-2").Return(nil)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"velero.io/backup-name":"foo"}},"status":{"phase":"InProgress"}}`),
			),
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"velero.io/backup-uid":"uid"}}}`),
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"Deleting"}}`),
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-1",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-2",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"Processed"}}`),
			),
			core.NewDeleteCollectionAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				pkgbackup.NewDeleteBackupRequestListOptions(td.req.Spec.BackupName, "uid"),
			),
		}

		velerotest.CompareActions(t, expectedActions, td.client.Actions())

		// Make sure snapshot was deleted
		assert.Equal(t, 0, td.volumeSnapshotter.SnapshotsTaken.Len())
	})

	t.Run("full delete, no errors, with backup name greater than 63 chars", func(t *testing.T) {
		backup := defaultBackup().
			ObjectMeta(
				builder.WithName("the-really-long-backup-name-that-is-much-more-than-63-characters"),
			).
			Result()
		backup.UID = "uid"
		backup.Spec.StorageLocation = "primary"

		restore1 := builder.ForRestore("velero", "restore-1").
			Phase(v1.RestorePhaseCompleted).
			Backup("the-really-long-backup-name-that-is-much-more-than-63-characters").
			Result()
		restore2 := builder.ForRestore("velero", "restore-2").
			Phase(v1.RestorePhaseCompleted).
			Backup("the-really-long-backup-name-that-is-much-more-than-63-characters").
			Result()
		restore3 := builder.ForRestore("velero", "restore-3").
			Phase(v1.RestorePhaseCompleted).
			Backup("some-other-backup").
			Result()

		td := setupBackupDeletionControllerTest(backup, restore1, restore2, restore3)
		td.req = pkgbackup.NewDeleteBackupRequest(backup.Name, string(backup.UID))
		td.req.Namespace = "velero"
		td.req.Name = "foo-abcde"
		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore1)
		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore2)
		td.sharedInformers.Velero().V1().Restores().Informer().GetStore().Add(restore3)

		location := &v1.BackupStorageLocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: backup.Namespace,
				Name:      backup.Spec.StorageLocation,
			},
			Spec: v1.BackupStorageLocationSpec{
				Provider: "objStoreProvider",
				StorageType: v1.StorageType{
					ObjectStorage: &v1.ObjectStorageLocation{
						Bucket: "bucket",
					},
				},
			},
		}
		require.NoError(t, td.sharedInformers.Velero().V1().BackupStorageLocations().Informer().GetStore().Add(location))

		snapshotLocation := &v1.VolumeSnapshotLocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: backup.Namespace,
				Name:      "vsl-1",
			},
			Spec: v1.VolumeSnapshotLocationSpec{
				Provider: "provider-1",
			},
		}
		require.NoError(t, td.sharedInformers.Velero().V1().VolumeSnapshotLocations().Informer().GetStore().Add(snapshotLocation))

		// Clear out req labels to make sure the controller adds them
		td.req.Labels = make(map[string]string)

		td.client.PrependReactor("get", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})
		td.volumeSnapshotter.SnapshotsTaken.Insert("snap-1")

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})

		td.client.PrependReactor("patch", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})

		snapshots := []*volume.Snapshot{
			{
				Spec: volume.SnapshotSpec{
					Location: "vsl-1",
				},
				Status: volume.SnapshotStatus{
					ProviderSnapshotID: "snap-1",
				},
			},
		}

		pluginManager := &pluginmocks.Manager{}
		pluginManager.On("GetVolumeSnapshotter", "provider-1").Return(td.volumeSnapshotter, nil)
		pluginManager.On("CleanupClients")
		td.controller.newPluginManager = func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager }

		td.backupStore.On("GetBackupVolumeSnapshots", td.req.Spec.BackupName).Return(snapshots, nil)
		td.backupStore.On("DeleteBackup", td.req.Spec.BackupName).Return(nil)
		td.backupStore.On("DeleteRestore", "restore-1").Return(nil)
		td.backupStore.On("DeleteRestore", "restore-2").Return(nil)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"velero.io/backup-name":"the-really-long-backup-name-that-is-much-more-than-63-cha6ca4bc"}},"status":{"phase":"InProgress"}}`),
			),
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"velero.io/backup-uid":"uid"}}}`),
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"Deleting"}}`),
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-1",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-2",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				types.MergePatchType,
				[]byte(`{"status":{"phase":"Processed"}}`),
			),
			core.NewDeleteCollectionAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				pkgbackup.NewDeleteBackupRequestListOptions(td.req.Spec.BackupName, "uid"),
			),
		}

		velerotest.CompareActions(t, expectedActions, td.client.Actions())

		// Make sure snapshot was deleted
		assert.Equal(t, 0, td.volumeSnapshotter.SnapshotsTaken.Len())
	})
}

func TestBackupDeletionControllerDeleteExpiredRequests(t *testing.T) {
	now := time.Date(2018, 4, 4, 12, 0, 0, 0, time.UTC)
	unexpired1 := time.Date(2018, 4, 4, 11, 0, 0, 0, time.UTC)
	unexpired2 := time.Date(2018, 4, 3, 12, 0, 1, 0, time.UTC)
	expired1 := time.Date(2018, 4, 3, 12, 0, 0, 0, time.UTC)
	expired2 := time.Date(2018, 4, 3, 2, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		requests          []*v1.DeleteBackupRequest
		expectedDeletions []string
	}{
		{
			name: "no requests",
		},
		{
			name: "older than max age, phase = '', don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: "",
					},
				},
			},
		},
		{
			name: "older than max age, phase = New, don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseNew,
					},
				},
			},
		},
		{
			name: "older than max age, phase = InProcess, don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseInProgress,
					},
				},
			},
		},
		{
			name: "some expired, some not",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "unexpired-1",
						CreationTimestamp: metav1.Time{Time: unexpired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "expired-1",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "unexpired-2",
						CreationTimestamp: metav1.Time{Time: unexpired2},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "expired-2",
						CreationTimestamp: metav1.Time{Time: expired2},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
			},
			expectedDeletions: []string{"expired-1", "expired-2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			sharedInformers := informers.NewSharedInformerFactory(client, 0)

			controller := NewBackupDeletionController(
				velerotest.NewLogger(),
				sharedInformers.Velero().V1().DeleteBackupRequests(),
				client.VeleroV1(), // deleteBackupRequestClient
				client.VeleroV1(), // backupClient
				sharedInformers.Velero().V1().Restores(),
				client.VeleroV1(), // restoreClient
				NewBackupTracker(),
				nil,
				sharedInformers.Velero().V1().PodVolumeBackups(),
				sharedInformers.Velero().V1().BackupStorageLocations(),
				sharedInformers.Velero().V1().VolumeSnapshotLocations(),
				nil, // new plugin manager func
				metrics.NewServerMetrics(),
			).(*backupDeletionController)

			fakeClock := &clock.FakeClock{}
			fakeClock.SetTime(now)
			controller.clock = fakeClock

			for i := range test.requests {
				sharedInformers.Velero().V1().DeleteBackupRequests().Informer().GetStore().Add(test.requests[i])
			}

			controller.deleteExpiredRequests()

			expectedActions := []core.Action{}
			for _, name := range test.expectedDeletions {
				expectedActions = append(expectedActions, core.NewDeleteAction(v1.SchemeGroupVersion.WithResource("deletebackuprequests"), "ns", name))
			}

			velerotest.CompareActions(t, expectedActions, client.Actions())
		})
	}
}
