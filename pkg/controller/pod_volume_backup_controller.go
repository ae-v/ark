/*
Copyright 2018 the Heptio Ark contributors.

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
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	arkv1api "github.com/heptio/ark/pkg/apis/ark/v1"
	arkv1client "github.com/heptio/ark/pkg/generated/clientset/versioned/typed/ark/v1"
	informers "github.com/heptio/ark/pkg/generated/informers/externalversions/ark/v1"
	listers "github.com/heptio/ark/pkg/generated/listers/ark/v1"
	"github.com/heptio/ark/pkg/restic"
	"github.com/heptio/ark/pkg/util/kube"
)

type podVolumeBackupController struct {
	*genericController

	podVolumeBackupClient arkv1client.PodVolumeBackupsGetter
	podVolumeBackupLister listers.PodVolumeBackupLister
	secretLister          corev1listers.SecretLister
	podLister             corev1listers.PodLister
	pvcLister             corev1listers.PersistentVolumeClaimLister
	nodeName              string

	processBackupFunc func(*arkv1api.PodVolumeBackup) error
}

// NewPodVolumeBackupController creates a new pod volume backup controller.
func NewPodVolumeBackupController(
	logger logrus.FieldLogger,
	podVolumeBackupInformer informers.PodVolumeBackupInformer,
	podVolumeBackupClient arkv1client.PodVolumeBackupsGetter,
	podInformer cache.SharedIndexInformer,
	secretInformer corev1informers.SecretInformer,
	pvcInformer corev1informers.PersistentVolumeClaimInformer,
	nodeName string,
) Interface {
	c := &podVolumeBackupController{
		genericController:     newGenericController("pod-volume-backup", logger),
		podVolumeBackupClient: podVolumeBackupClient,
		podVolumeBackupLister: podVolumeBackupInformer.Lister(),
		podLister:             corev1listers.NewPodLister(podInformer.GetIndexer()),
		secretLister:          secretInformer.Lister(),
		pvcLister:             pvcInformer.Lister(),
		nodeName:              nodeName,
	}

	c.syncHandler = c.processQueueItem
	c.cacheSyncWaiters = append(
		c.cacheSyncWaiters,
		podVolumeBackupInformer.Informer().HasSynced,
		secretInformer.Informer().HasSynced,
		podInformer.HasSynced,
		pvcInformer.Informer().HasSynced,
	)
	c.processBackupFunc = c.processBackup

	podVolumeBackupInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
		},
	)

	return c
}

func (c *podVolumeBackupController) processQueueItem(key string) error {
	log := c.logger.WithField("key", key)
	log.Debug("Running processItem")

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		log.WithError(err).Error("error splitting queue key")
		return nil
	}

	req, err := c.podVolumeBackupLister.PodVolumeBackups(ns).Get(name)
	if apierrors.IsNotFound(err) {
		log.Debug("Unable to find PodVolumeBackup")
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "error getting PodVolumeBackup")
	}

	// only process new items
	switch req.Status.Phase {
	case "", arkv1api.PodVolumeBackupPhaseNew:
	default:
		return nil
	}

	// only process items for this node
	if req.Spec.Node != c.nodeName {
		return nil
	}

	// Don't mutate the shared cache
	reqCopy := req.DeepCopy()
	return c.processBackupFunc(reqCopy)
}

func (c *podVolumeBackupController) processBackup(req *arkv1api.PodVolumeBackup) error {
	log := c.logger.WithFields(logrus.Fields{
		"namespace": req.Namespace,
		"name":      req.Name,
	})

	var err error

	// update status to InProgress
	req, err = c.patchPodVolumeBackup(req, updatePhaseFunc(arkv1api.PodVolumeBackupPhaseInProgress))
	if err != nil {
		log.WithError(err).Error("Error setting phase to InProgress")
		return errors.WithStack(err)
	}

	pod, err := c.podLister.Pods(req.Spec.Pod.Namespace).Get(req.Spec.Pod.Name)
	if err != nil {
		log.WithError(err).Errorf("Error getting pod %s/%s", req.Spec.Pod.Namespace, req.Spec.Pod.Name)
		return c.fail(req, errors.Wrap(err, "error getting pod").Error(), log)
	}

	volumeDir, err := kube.GetVolumeDirectory(pod, req.Spec.Volume, c.pvcLister)
	if err != nil {
		log.WithError(err).Error("Error getting volume directory name")
		return c.fail(req, errors.Wrap(err, "error getting volume directory name").Error(), log)
	}

	path, err := singlePathMatch(fmt.Sprintf("/host_pods/%s/volumes/*/%s", string(req.Spec.Pod.UID), volumeDir))
	if err != nil {
		log.WithError(err).Error("Error uniquely identifying volume path")
		return c.fail(req, errors.Wrap(err, "error getting volume path on host").Error(), log)
	}

	// temp creds
	file, err := restic.TempCredentialsFile(c.secretLister, req.Spec.Pod.Namespace)
	if err != nil {
		log.WithError(err).Error("Error creating temp restic credentials file")
		return c.fail(req, errors.Wrap(err, "error creating temp restic credentials file").Error(), log)
	}
	// ignore error since there's nothing we can do and it's a temp file.
	defer os.Remove(file)

	resticCmd := restic.BackupCommand(
		req.Spec.RepoPrefix,
		req.Spec.Pod.Namespace,
		file,
		path,
		req.Spec.Tags,
	)

	var stdout, stderr string

	if stdout, stderr, err = runCommand(resticCmd.Cmd()); err != nil {
		log.WithError(errors.WithStack(err)).Errorf("Error running command=%s, stdout=%s, stderr=%s", resticCmd.String(), stdout, stderr)
		return c.fail(req, fmt.Sprintf("error running restic backup, stderr=%s: %s", stderr, err.Error()), log)
	}
	log.Debugf("Ran command=%s, stdout=%s, stderr=%s", resticCmd.String(), stdout, stderr)

	snapshotID, err := restic.GetSnapshotID(req.Spec.RepoPrefix, req.Spec.Pod.Namespace, file, req.Spec.Tags)
	if err != nil {
		log.WithError(err).Error("Error getting SnapshotID")
		return c.fail(req, errors.Wrap(err, "error getting snapshot id").Error(), log)
	}

	// update status to Completed with path & snapshot id
	req, err = c.patchPodVolumeBackup(req, func(r *arkv1api.PodVolumeBackup) {
		r.Status.Path = path
		r.Status.SnapshotID = snapshotID
		r.Status.Phase = arkv1api.PodVolumeBackupPhaseCompleted
	})
	if err != nil {
		log.WithError(err).Error("Error setting phase to Completed")
		return err
	}

	return nil
}

// runCommand runs a command and returns its stdout, stderr, and its returned
// error (if any). If there are errors reading stdout or stderr, their return
// value(s) will contain the error as a string.
func runCommand(cmd *exec.Cmd) (string, string, error) {
	stdoutBuf := new(bytes.Buffer)
	stderrBuf := new(bytes.Buffer)

	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	runErr := cmd.Run()

	var stdout, stderr string

	if res, readErr := ioutil.ReadAll(stdoutBuf); readErr != nil {
		stdout = errors.Wrap(readErr, "error reading command's stdout").Error()
	} else {
		stdout = string(res)
	}

	if res, readErr := ioutil.ReadAll(stderrBuf); readErr != nil {
		stderr = errors.Wrap(readErr, "error reading command's stderr").Error()
	} else {
		stderr = string(res)
	}

	return stdout, stderr, runErr
}

func (c *podVolumeBackupController) patchPodVolumeBackup(req *arkv1api.PodVolumeBackup, mutate func(*arkv1api.PodVolumeBackup)) (*arkv1api.PodVolumeBackup, error) {
	// Record original json
	oldData, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "error marshalling original PodVolumeBackup")
	}

	// Mutate
	mutate(req)

	// Record new json
	newData, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "error marshalling updated PodVolumeBackup")
	}

	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return nil, errors.Wrap(err, "error creating json merge patch for PodVolumeBackup")
	}

	req, err = c.podVolumeBackupClient.PodVolumeBackups(req.Namespace).Patch(req.Name, types.MergePatchType, patchBytes)
	if err != nil {
		return nil, errors.Wrap(err, "error patching PodVolumeBackup")
	}

	return req, nil
}

func (c *podVolumeBackupController) fail(req *arkv1api.PodVolumeBackup, msg string, log logrus.FieldLogger) error {
	if _, err := c.patchPodVolumeBackup(req, func(r *arkv1api.PodVolumeBackup) {
		r.Status.Phase = arkv1api.PodVolumeBackupPhaseFailed
		r.Status.Message = msg
	}); err != nil {
		log.WithError(err).Error("Error setting phase to Failed")
		return err
	}
	return nil
}

func updatePhaseFunc(phase arkv1api.PodVolumeBackupPhase) func(r *arkv1api.PodVolumeBackup) {
	return func(r *arkv1api.PodVolumeBackup) {
		r.Status.Phase = phase
	}
}

func singlePathMatch(path string) (string, error) {
	matches, err := filepath.Glob(path)
	if err != nil {
		return "", errors.WithStack(err)
	}

	if len(matches) != 1 {
		return "", errors.Errorf("expected one matching path, got %d", len(matches))
	}

	return matches[0], nil
}
