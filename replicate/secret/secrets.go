package secret

import (
	"encoding/json"
	"fmt"
	"github.com/mittwald/kubernetes-replicator/replicate/common"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/types"
	"sort"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Replicator struct {
	*common.GenericReplicator
}

// NewReplicator creates a new secret replicator
func NewReplicator(client kubernetes.Interface, resyncPeriod time.Duration, allowAll bool) common.Replicator {
	repl := Replicator{
		GenericReplicator: common.NewGenericReplicator(common.ReplicatorConfig{
			Kind:         "Secret",
			ObjType:      &v1.Secret{},
			AllowAll:     allowAll,
			ResyncPeriod: resyncPeriod,
			Client:       client,
			ListFunc: func(lo metav1.ListOptions) (runtime.Object, error) {
				return client.CoreV1().Secrets("").List(lo)
			},
			WatchFunc: func(lo metav1.ListOptions) (watch.Interface, error) {
				return client.CoreV1().Secrets("").Watch(lo)
			},
		}),
	}
	repl.UpdateFuncs = common.UpdateFuncs{
		ReplicateDataFrom:        repl.ReplicateDataFrom,
		ReplicateObjectTo:        repl.ReplicateObjectTo,
		PatchDeleteDependent:     repl.PatchDeleteDependent,
		DeleteReplicatedResource: repl.DeleteReplicatedResource,
	}

	return &repl
}

// ReplicateDataFrom takes a source object and copies over data to target object
func (r *Replicator) ReplicateDataFrom(sourceObj interface{}, targetObj interface{}) error {
	source := sourceObj.(*v1.Secret)
	target := targetObj.(*v1.Secret)

	// make sure replication is allowed
	logger := log.
		WithField("kind", r.Kind).
		WithField("source", common.MustGetKey(source)).
		WithField("target", common.MustGetKey(target))

	if ok, err := r.IsReplicationPermitted(&target.ObjectMeta, &source.ObjectMeta); !ok {
		return errors.Wrapf(err, "replication of target %s is not permitted", common.MustGetKey(source))
	}

	targetVersion, ok := target.Annotations[common.ReplicatedFromVersionAnnotation]
	sourceVersion := source.ResourceVersion

	if ok && targetVersion == sourceVersion {
		logger.Debugf("target %s is already up-to-date", common.MustGetKey(target))
		return nil
	}

	targetCopy := target.DeepCopy()

	if targetCopy.Data == nil {
		targetCopy.Data = make(map[string][]byte)
	}

	prevKeys, hasPrevKeys := common.PreviouslyPresentKeys(&targetCopy.ObjectMeta)
	replicatedKeys := make([]string, 0)

	for key, value := range source.Data {
		newValue := make([]byte, len(value))
		copy(newValue, value)
		targetCopy.Data[key] = newValue

		replicatedKeys = append(replicatedKeys, key)
		delete(prevKeys, key)
	}

	if hasPrevKeys {
		for k := range prevKeys {
			logger.Debugf("removing previously present key %s: not present in source any more", k)
			delete(targetCopy.Data, k)
		}
	}

	sort.Strings(replicatedKeys)

	logger.Infof("updating target %s", common.MustGetKey(target))

	targetCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	targetCopy.Annotations[common.ReplicatedFromVersionAnnotation] = source.ResourceVersion
	targetCopy.Annotations[common.ReplicatedKeysAnnotation] = strings.Join(replicatedKeys, ",")

	s, err := r.Client.CoreV1().Secrets(target.Namespace).Update(targetCopy)
	if err != nil {
		return errors.Wrapf(err, "Failed updating target %s/%s", target.Namespace, targetCopy.Name)
	}

	if err := r.Store.Update(s); err != nil {
		return errors.Wrapf(err, "Failed to update cache for %s/%s: %v", target.Namespace, targetCopy, err)
	}

	return nil
}

// ReplicateObjectTo copies the whole object to target namespace
func (r *Replicator) ReplicateObjectTo(sourceObj interface{}, target *v1.Namespace) error {
	source := sourceObj.(*v1.Secret)
	targetLocation := fmt.Sprintf("%s/%s", target.Name, source.Name)

	logger := log.
		WithField("kind", r.Kind).
		WithField("source", common.MustGetKey(source)).
		WithField("target", targetLocation)

	targetResourceType := source.Type
	targetResource, exists, err := r.Store.GetByKey(targetLocation)
	if err != nil {
		return errors.Wrapf(err, "Could not get %s from cache!", targetLocation)
	}
	logger.Infof("Checking if %s exists? %v", targetLocation, exists)

	var resourceCopy *v1.Secret
	if exists {
		targetObject := targetResource.(*v1.Secret)
		targetVersion, ok := targetObject.Annotations[common.ReplicatedFromVersionAnnotation]
		sourceVersion := source.ResourceVersion

		if ok && targetVersion == sourceVersion {
			logger.Debugf("Secret %s is already up-to-date", common.MustGetKey(targetObject))
			return nil
		}

		targetResourceType = targetObject.Type
		resourceCopy = targetObject.DeepCopy()
	} else {
		resourceCopy = new(v1.Secret)
	}

	if resourceCopy.Data == nil {
		resourceCopy.Data = make(map[string][]byte)
	}
	if resourceCopy.Annotations == nil {
		resourceCopy.Annotations = make(map[string]string)
	}

	prevKeys, hasPrevKeys := common.PreviouslyPresentKeys(&resourceCopy.ObjectMeta)
	replicatedKeys := make([]string, 0)

	for key, value := range source.Data {
		newValue := make([]byte, len(value))
		copy(newValue, value)
		resourceCopy.Data[key] = newValue

		replicatedKeys = append(replicatedKeys, key)
		delete(prevKeys, key)
	}

	if hasPrevKeys {
		for k := range prevKeys {
			logger.Debugf("removing previously present key %s: not present in source secret any more", k)
			delete(resourceCopy.Data, k)
		}
	}

	sort.Strings(replicatedKeys)
	resourceCopy.Name = source.Name
	resourceCopy.Type = targetResourceType
	resourceCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	resourceCopy.Annotations[common.ReplicatedFromVersionAnnotation] = source.ResourceVersion
	resourceCopy.Annotations[common.ReplicatedKeysAnnotation] = strings.Join(replicatedKeys, ",")

	var obj interface{}
	if exists {
		logger.Debugf("Updating existing secret %s/%s", target.Name, resourceCopy.Name)
		obj, err = r.Client.CoreV1().Secrets(target.Name).Update(resourceCopy)
	} else {
		logger.Debugf("Creating a new secret secret %s/%s", target.Name, resourceCopy.Name)
		obj, err = r.Client.CoreV1().Secrets(target.Name).Create(resourceCopy)
	}
	if err != nil {
		return errors.Wrapf(err, "Failed to update secret %s/%s", target.Name, resourceCopy.Name)
	}

	if err := r.Store.Update(obj); err != nil {
		return errors.Wrapf(err, "Failed to update cache for %s/%s", target.Name, resourceCopy)
	}

	return nil
}

func (r *Replicator) PatchDeleteDependent(sourceKey string, target interface{}) (interface{}, error) {
	dependentKey := common.MustGetKey(target)
	logger := log.WithFields(log.Fields{
		"kind":   r.Kind,
		"source": sourceKey,
		"target": dependentKey,
	})

	targetObject, ok := target.(*v1.Secret)
	if !ok {
		err := errors.Errorf("bad type returned from Store: %T", target)
		return nil, err
	}

	patch := []common.JSONPatchOperation{{Operation: "remove", Path: "/data"}}
	patchBody, err := json.Marshal(&patch)

	if err != nil {
		return nil, errors.Wrapf(err, "error while building patch body for secret %s: %v", dependentKey, err)
	}

	logger.Debugf("clearing dependent %s %s", r.Kind, dependentKey)
	logger.Tracef("patch body: %s", string(patchBody))

	s, err := r.Client.CoreV1().Secrets(targetObject.Namespace).Patch(targetObject.Name, types.JSONPatchType, patchBody)
	if err != nil {
		return nil, errors.Wrapf(err, "error while patching secret %s: %v", dependentKey, err)
	}
	return s, nil
}

// DeleteReplicatedResource deletes a resource replicated by ReplicateTo annotation
func (r *Replicator) DeleteReplicatedResource(targetResource interface{}) error {
	targetLocation := common.MustGetKey(targetResource)
	logger := log.WithFields(log.Fields{
		"kind":   r.Kind,
		"target": targetLocation,
	})

	object := targetResource.(*v1.Secret)
	resourceKeys := strings.Join(common.GetKeysFromBinaryMap(object.Data), ",")
	if resourceKeys == object.Annotations[common.ReplicatedKeysAnnotation] {
		logger.Debugf("Deleting %s", targetLocation)
		if err := r.Client.CoreV1().Secrets(object.Namespace).Delete(object.Name, &metav1.DeleteOptions{}); err != nil {
			return errors.Wrapf(err, "Failed deleting %s: %v", targetLocation, err)
		}
	} else {
		logger.Debugf("Not deleting %s since it contains other keys then replicated.", targetLocation)
	}

	return nil
}
