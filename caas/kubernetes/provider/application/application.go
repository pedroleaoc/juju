// Copyright 2020 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package application

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/juju/clock"
	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils/v2/arch"
	"github.com/kr/pretty"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/juju/juju/caas"
	"github.com/juju/juju/caas/kubernetes/provider/constants"
	"github.com/juju/juju/caas/kubernetes/provider/resources"
	"github.com/juju/juju/caas/kubernetes/provider/storage"
	k8sutils "github.com/juju/juju/caas/kubernetes/provider/utils"
	k8swatcher "github.com/juju/juju/caas/kubernetes/provider/watcher"
	"github.com/juju/juju/core/annotations"
	"github.com/juju/juju/core/paths"
	"github.com/juju/juju/core/status"
	"github.com/juju/juju/core/watcher"
	jujustorage "github.com/juju/juju/storage"
)

var logger = loggo.GetLogger("juju.kubernetes.provider.application")

const (
	unitContainerName            = "charm"
	charmVolumeName              = "charm-data"
	agentProbeInitialDelay int32 = 30
	agentProbePeriod       int32 = 10
	agentProbeSuccess      int32 = 1
	agentProbeFailure      int32 = 2
)

type app struct {
	name           string
	namespace      string
	modelUUID      string
	modelName      string
	legacyLabels   bool
	deploymentType caas.DeploymentType
	client         kubernetes.Interface
	newWatcher     k8swatcher.NewK8sWatcherFunc
	clock          clock.Clock

	// randomPrefix generates an annotation for stateful sets.
	randomPrefix k8sutils.RandomPrefixFunc

	newApplier func() resources.Applier
}

// NewApplication returns an application.
func NewApplication(
	name string,
	namespace string,
	modelUUID string,
	modelName string,
	legacyLabels bool,
	deploymentType caas.DeploymentType,
	client kubernetes.Interface,
	newWatcher k8swatcher.NewK8sWatcherFunc,
	clock clock.Clock,
	randomPrefix k8sutils.RandomPrefixFunc,
) caas.Application {
	return newApplication(
		name,
		namespace,
		modelUUID,
		modelName,
		legacyLabels,
		deploymentType,
		client,
		newWatcher,
		clock,
		randomPrefix,
		resources.NewApplier,
	)
}

func newApplication(
	name string,
	namespace string,
	modelUUID string,
	modelName string,
	legacyLabels bool,
	deploymentType caas.DeploymentType,
	client kubernetes.Interface,
	newWatcher k8swatcher.NewK8sWatcherFunc,
	clock clock.Clock,
	randomPrefix k8sutils.RandomPrefixFunc,
	newApplier func() resources.Applier,
) caas.Application {
	return &app{
		name:           name,
		namespace:      namespace,
		modelUUID:      modelUUID,
		modelName:      modelName,
		legacyLabels:   legacyLabels,
		deploymentType: deploymentType,
		client:         client,
		newWatcher:     newWatcher,
		clock:          clock,
		randomPrefix:   randomPrefix,
		newApplier:     newApplier,
	}
}

// Ensure creates or updates an application pod with the given application
// name, agent path, and application config.
func (a *app) Ensure(config caas.ApplicationConfig) (err error) {
	// TODO: add support `numUnits`, `Constraints` and `Devices`.
	// TODO: storage handling for deployment/daemonset enhancement.
	defer func() {
		if err != nil {
			logger.Errorf("Ensure %s", err)
		}
	}()
	logger.Debugf("creating/updating %s application", a.name)

	applier := a.newApplier()
	secret := resources.Secret{
		Secret: corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        a.secretName(),
				Namespace:   a.namespace,
				Labels:      a.labels(),
				Annotations: a.annotations(config),
			},
			Data: map[string][]byte{
				"JUJU_K8S_APPLICATION":          []byte(a.name),
				"JUJU_K8S_MODEL":                []byte(a.modelUUID),
				"JUJU_K8S_APPLICATION_PASSWORD": []byte(config.IntroductionSecret),
				"JUJU_K8S_CONTROLLER_ADDRESSES": []byte(config.ControllerAddresses),
				"JUJU_K8S_CONTROLLER_CA_CERT":   []byte(config.ControllerCertBundle),
			},
		},
	}
	applier.Apply(&secret)

	if err := a.configureDefaultService(a.annotations(config)); err != nil {
		return errors.Annotatef(err, "ensuring the default service %q", a.name)
	}

	// Set up the parameters for creating charm storage (if required).
	podSpec, err := a.applicationPodSpec(config)
	if err != nil {
		return errors.Annotate(err, "generating application podspec")
	}

	var handleVolume handleVolumeFunc = func(v corev1.Volume, mountPath string, readOnly bool) (*corev1.VolumeMount, error) {
		if err := storage.PushUniqueVolume(podSpec, v, false); err != nil {
			return nil, errors.Trace(err)
		}
		return &corev1.VolumeMount{
			Name:      v.Name,
			ReadOnly:  readOnly,
			MountPath: mountPath,
		}, nil
	}
	var handleVolumeMount handleVolumeMountFunc = func(storageName string, m corev1.VolumeMount) error {
		for i := range podSpec.Containers {
			name := podSpec.Containers[i].Name
			if name == unitContainerName {
				podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, m)
				continue
			}
			for _, mount := range config.Containers[name].Mounts {
				if mount.StorageName == storageName {
					volumeMountCopy := m
					// TODO(embedded): volumeMountCopy.MountPath was defined in `caas.ApplicationConfig.Filesystems[*].Attachment.Path`.
					// Consolidate `caas.ApplicationConfig.Filesystems[*].Attachment.Path` and `caas.ApplicationConfig.Containers[*].Mounts[*].Path`!!!
					volumeMountCopy.MountPath = mount.Path
					podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, volumeMountCopy)
				}
			}
		}
		return nil
	}
	var handlePVCForStatelessResource handlePVCFunc = func(pvc corev1.PersistentVolumeClaim, mountPath string, readOnly bool) (*corev1.VolumeMount, error) {
		// Ensure PVC.
		r := resources.NewPersistentVolumeClaim(pvc.GetName(), a.namespace, &pvc)
		applier.Apply(r)

		// Push the volume to podspec.
		vol := corev1.Volume{
			Name: r.GetName(),
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: r.GetName(),
					ReadOnly:  readOnly,
				},
			},
		}
		return handleVolume(vol, mountPath, readOnly)
	}
	storageClasses, err := resources.ListStorageClass(context.Background(), a.client, metav1.ListOptions{})
	if err != nil {
		return errors.Trace(err)
	}
	var handleStorageClass = func(sc storagev1.StorageClass) error {
		applier.Apply(&resources.StorageClass{StorageClass: sc})
		return nil
	}
	var configureStorage = func(storageUniqueID string, handlePVC handlePVCFunc) error {
		err := a.configureStorage(
			storageUniqueID,
			config.Filesystems,
			storageClasses,
			handleVolume, handleVolumeMount, handlePVC, handleStorageClass,
		)
		return errors.Trace(err)
	}

	switch a.deploymentType {
	case caas.DeploymentStateful:
		if err := a.configureHeadlessService(a.name, a.annotations(config)); err != nil {
			return errors.Annotatef(err, "creating or updating headless service for %q %q", a.deploymentType, a.name)
		}
		exists := true
		ss, getErr := a.getStatefulSet()
		if errors.IsNotFound(getErr) {
			exists = false
		} else if getErr != nil {
			return errors.Trace(getErr)
		}
		storageUniqueID, err := a.getStorageUniqPrefix(func() (annotationGetter, error) {
			return ss, getErr
		})
		if err != nil {
			return errors.Trace(err)
		}
		var numPods *int32
		if !exists {
			numPods = int32Ptr(1)
		}
		statefulset := resources.StatefulSet{
			StatefulSet: appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      a.name,
					Namespace: a.namespace,
					Labels:    a.labels(),
					Annotations: a.annotations(config).
						Add(k8sutils.AnnotationKeyApplicationUUID(false), storageUniqueID),
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: numPods,
					Selector: &metav1.LabelSelector{
						MatchLabels: a.selectorLabels(),
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      a.selectorLabels(),
							Annotations: a.annotations(config),
						},
						Spec: *podSpec,
					},
					PodManagementPolicy: appsv1.ParallelPodManagement,
				},
			},
		}

		if err = configureStorage(
			storageUniqueID,
			func(pvc corev1.PersistentVolumeClaim, mountPath string, readOnly bool) (*corev1.VolumeMount, error) {
				if err := storage.PushUniqueVolumeClaimTemplate(&statefulset.Spec, pvc); err != nil {
					return nil, errors.Trace(err)
				}
				return &corev1.VolumeMount{
					Name:      pvc.GetName(),
					ReadOnly:  readOnly,
					MountPath: mountPath,
				}, nil
			},
		); err != nil {
			return errors.Trace(err)
		}

		applier.Apply(&statefulset)
	case caas.DeploymentStateless:
		exists := true
		d, getErr := a.getDeployment()
		if errors.IsNotFound(getErr) {
			exists = false
		} else if getErr != nil {
			return errors.Trace(getErr)
		}
		storageUniqueID, err := a.getStorageUniqPrefix(func() (annotationGetter, error) {
			return d, getErr
		})
		if err != nil {
			return errors.Trace(err)
		}
		var numPods *int32
		if !exists {
			numPods = int32Ptr(1)
		}
		// Config storage to update the podspec with storage info.
		if err = configureStorage(storageUniqueID, handlePVCForStatelessResource); err != nil {
			return errors.Trace(err)
		}
		deployment := resources.Deployment{
			Deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      a.name,
					Namespace: a.namespace,
					Labels:    a.labels(),
					Annotations: a.annotations(config).
						Add(k8sutils.AnnotationKeyApplicationUUID(false), storageUniqueID),
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: numPods,
					Selector: &metav1.LabelSelector{
						MatchLabels: a.selectorLabels(),
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      a.selectorLabels(),
							Annotations: a.annotations(config),
						},
						Spec: *podSpec,
					},
				},
			},
		}

		applier.Apply(&deployment)
	case caas.DeploymentDaemon:
		storageUniqueID, err := a.getStorageUniqPrefix(func() (annotationGetter, error) {
			return a.getDaemonSet()
		})
		if err != nil {
			return errors.Trace(err)
		}
		// Config storage to update the podspec with storage info.
		if err = configureStorage(storageUniqueID, handlePVCForStatelessResource); err != nil {
			return errors.Trace(err)
		}
		daemonset := resources.DaemonSet{
			DaemonSet: appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      a.name,
					Namespace: a.namespace,
					Labels:    a.labels(),
					Annotations: a.annotations(config).
						Add(k8sutils.AnnotationKeyApplicationUUID(false), storageUniqueID),
				},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: a.selectorLabels(),
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      a.selectorLabels(),
							Annotations: a.annotations(config),
						},
						Spec: *podSpec,
					},
				},
			},
		}
		applier.Apply(&daemonset)
	default:
		return errors.NotSupportedf("unknown deployment type")
	}

	return applier.Run(context.Background(), a.client, false)
}

// Exists indicates if the application for the specified
// application exists, and whether the application is terminating.
func (a *app) Exists() (caas.DeploymentState, error) {
	checks := []struct {
		label            string
		check            func() (bool, bool, error)
		forceTerminating bool
	}{
		{},
		{"secret", a.secretExists, false},
		{"service", a.serviceExists, false},
	}
	switch a.deploymentType {
	case caas.DeploymentStateful:
		checks[0].label = "statefulset"
		checks[0].check = a.statefulSetExists
	case caas.DeploymentStateless:
		checks[0].label = "deployment"
		checks[0].check = a.deploymentExists
	case caas.DeploymentDaemon:
		checks[0].label = "daemonset"
		checks[0].check = a.daemonSetExists
	default:
		return caas.DeploymentState{}, errors.NotSupportedf("unknown deployment type")
	}

	state := caas.DeploymentState{}
	for _, c := range checks {
		exists, terminating, err := c.check()
		if err != nil {
			return caas.DeploymentState{}, errors.Annotatef(err, "%s resource check", c.label)
		}
		if !exists {
			continue
		}
		state.Exists = true
		if terminating || c.forceTerminating {
			// Terminating is always set to true regardless of whether the resource is failed as terminating
			// since it's the overall state that is reported baca.
			logger.Debugf("application %q exists and is terminating due to dangling %s resource(s)", a.name, c.label)
			return caas.DeploymentState{Exists: true, Terminating: true}, nil
		}
	}
	return state, nil
}

func headlessServiceName(appName string) string {
	return fmt.Sprintf("%s-endpoints", appName)
}

func (a *app) configureHeadlessService(name string, annotation annotations.Annotation) error {
	svc := resources.NewService(headlessServiceName(name), a.namespace, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: a.labels(),
			Annotations: annotation.
				Add("service.alpha.kubernetes.io/tolerate-unready-endpoints", "true"),
		},
		Spec: corev1.ServiceSpec{
			Selector:                 a.selectorLabels(),
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                "None",
			PublishNotReadyAddresses: true,
		},
	})
	return svc.Apply(context.Background(), a.client)
}

// configureDefaultService configures the default service for the application.
// It's only configured once when the application was deployed in the first time.
func (a *app) configureDefaultService(annotation annotations.Annotation) (err error) {
	svc := resources.NewService(a.name, a.namespace, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      a.labels(),
			Annotations: annotation,
		},
		Spec: corev1.ServiceSpec{
			Selector: a.selectorLabels(),
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name: "placeholder",
				Port: 65535,
			}},
		},
	})
	if err = svc.Get(context.Background(), a.client); errors.IsNotFound(err) {
		return svc.Apply(context.Background(), a.client)
	}
	return errors.Trace(err)
}

// UpdateService updates the default service with specific service type and port mappings.
func (a *app) UpdateService(param caas.ServiceParam) error {
	// This method will be used for juju [un]expose.
	// TODO(embedded): it might be changed later when we have proper modelling for the juju expose for the embedded charms.
	svc, err := a.getService()
	if err != nil {
		return errors.Annotatef(err, "getting existing service %q", a.name)
	}
	svc.Service.Spec.Type = corev1.ServiceType(param.Type)
	svc.Service.Spec.Ports = make([]corev1.ServicePort, len(param.Ports))
	for i, p := range param.Ports {
		svc.Service.Spec.Ports[i] = convertServicePort(p)
	}

	applier := a.newApplier()
	applier.Apply(svc)
	if err := a.updateContainerPorts(applier, svc.Service.Spec.Ports); err != nil {
		return errors.Trace(err)
	}
	return applier.Run(context.Background(), a.client, false)
}

func convertServicePort(p caas.ServicePort) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       p.Name,
		Port:       int32(p.Port),
		TargetPort: intstr.FromInt(p.TargetPort),
		Protocol:   corev1.Protocol(p.Protocol),
	}
}

func (a *app) getService() (*resources.Service, error) {
	svc := resources.NewService(a.name, a.namespace, nil)
	if err := svc.Get(context.Background(), a.client); err != nil {
		return nil, errors.Trace(err)
	}
	return svc, nil
}

// UpdatePorts updates port mappings on the specified service.
func (a *app) UpdatePorts(ports []caas.ServicePort, updateContainerPorts bool) error {
	svc, err := a.getService()
	if err != nil {
		return errors.Annotatef(err, "getting existing service %q", a.name)
	}
	svc.Service.Spec.Ports = make([]corev1.ServicePort, len(ports))
	for i, port := range ports {
		svc.Service.Spec.Ports[i] = convertServicePort(port)
	}
	applier := a.newApplier()
	applier.Apply(svc)

	if updateContainerPorts {
		if err := a.updateContainerPorts(applier, svc.Service.Spec.Ports); err != nil {
			return errors.Trace(err)
		}
	}
	err = applier.Run(context.Background(), a.client, false)
	return errors.Trace(err)
}

func convertContainerPort(p corev1.ServicePort) corev1.ContainerPort {
	return corev1.ContainerPort{
		Name:          p.Name,
		ContainerPort: p.TargetPort.IntVal,
		Protocol:      p.Protocol,
	}
}

func (a *app) updateContainerPorts(applier resources.Applier, ports []corev1.ServicePort) error {
	updatePodSpec := func(spec *corev1.PodSpec, containerPorts []corev1.ContainerPort) {
		for i, c := range spec.Containers {
			ps := containerPorts
			if c.Name != unitContainerName {
				spec.Containers[i].Ports = ps
			}
		}
	}

	containerPorts := make([]corev1.ContainerPort, len(ports))
	for i, p := range ports {
		containerPorts[i] = convertContainerPort(p)
	}

	switch a.deploymentType {
	case caas.DeploymentStateful:
		ss := resources.NewStatefulSet(a.name, a.namespace, nil)
		if err := ss.Get(context.Background(), a.client); err != nil {
			return errors.Trace(err)
		}

		updatePodSpec(&ss.StatefulSet.Spec.Template.Spec, containerPorts)
		applier.Apply(ss)
	case caas.DeploymentStateless:
		d := resources.NewDeployment(a.name, a.namespace, nil)
		if err := d.Get(context.Background(), a.client); err != nil {
			return errors.Trace(err)
		}

		updatePodSpec(&d.Deployment.Spec.Template.Spec, containerPorts)
		applier.Apply(d)
	case caas.DeploymentDaemon:
		d := resources.NewDaemonSet(a.name, a.namespace, nil)
		if err := d.Get(context.Background(), a.client); err != nil {
			return errors.Trace(err)
		}

		updatePodSpec(&d.DaemonSet.Spec.Template.Spec, containerPorts)
		applier.Apply(d)
	default:
		return errors.NotSupportedf("unknown deployment type")
	}
	return nil
}

func (a *app) getStatefulSet() (*resources.StatefulSet, error) {
	ss := resources.NewStatefulSet(a.name, a.namespace, nil)
	if err := ss.Get(context.Background(), a.client); err != nil {
		return nil, err
	}
	return ss, nil
}

func (a *app) getDeployment() (*resources.Deployment, error) {
	ss := resources.NewDeployment(a.name, a.namespace, nil)
	if err := ss.Get(context.Background(), a.client); err != nil {
		return nil, err
	}
	return ss, nil
}

func (a *app) getDaemonSet() (*resources.DaemonSet, error) {
	ss := resources.NewDaemonSet(a.name, a.namespace, nil)
	if err := ss.Get(context.Background(), a.client); err != nil {
		return nil, err
	}
	return ss, nil
}

func (a *app) statefulSetExists() (exists bool, terminating bool, err error) {
	ss := resources.NewStatefulSet(a.name, a.namespace, nil)
	err = ss.Get(context.Background(), a.client)
	if errors.IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, errors.Trace(err)
	}
	return true, ss.DeletionTimestamp != nil, nil
}

func (a *app) deploymentExists() (exists bool, terminating bool, err error) {
	ss := resources.NewDeployment(a.name, a.namespace, nil)
	err = ss.Get(context.Background(), a.client)
	if errors.IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, errors.Trace(err)
	}
	return true, ss.DeletionTimestamp != nil, nil
}

func (a *app) daemonSetExists() (exists bool, terminating bool, err error) {
	ss := resources.NewDaemonSet(a.name, a.namespace, nil)
	err = ss.Get(context.Background(), a.client)
	if errors.IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, errors.Trace(err)
	}
	return true, ss.DeletionTimestamp != nil, nil
}

func (a *app) secretExists() (exists bool, terminating bool, err error) {
	ss := resources.NewSecret(a.secretName(), a.namespace, nil)
	err = ss.Get(context.Background(), a.client)
	if errors.IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, errors.Trace(err)
	}
	return true, ss.DeletionTimestamp != nil, nil
}

func (a *app) serviceExists() (exists bool, terminating bool, err error) {
	ss := resources.NewService(a.name, a.namespace, nil)
	err = ss.Get(context.Background(), a.client)
	if errors.IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, errors.Trace(err)
	}
	return true, ss.DeletionTimestamp != nil, nil
}

// Delete deletes the specified application.
func (a *app) Delete() error {
	logger.Debugf("deleting %s application", a.name)
	applier := a.newApplier()
	switch a.deploymentType {
	case caas.DeploymentStateful:
		applier.Delete(resources.NewStatefulSet(a.name, a.namespace, nil))
		applier.Delete(resources.NewService(headlessServiceName(a.name), a.namespace, nil))
	case caas.DeploymentStateless:
		applier.Delete(resources.NewDeployment(a.name, a.namespace, nil))
	case caas.DeploymentDaemon:
		applier.Delete(resources.NewDaemonSet(a.name, a.namespace, nil))
	default:
		return errors.NotSupportedf("unknown deployment type")
	}
	applier.Delete(resources.NewService(a.name, a.namespace, nil))
	applier.Delete(resources.NewSecret(a.secretName(), a.namespace, nil))
	return applier.Run(context.Background(), a.client, false)
}

// Watch returns a watcher which notifies when there
// are changes to the application of the specified application.
func (a *app) Watch() (watcher.NotifyWatcher, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, 0,
		informers.WithNamespace(a.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = a.fieldSelector()
		}),
	)
	var informer cache.SharedIndexInformer
	switch a.deploymentType {
	case caas.DeploymentStateful:
		informer = factory.Apps().V1().StatefulSets().Informer()
	case caas.DeploymentStateless:
		informer = factory.Apps().V1().Deployments().Informer()
	case caas.DeploymentDaemon:
		informer = factory.Apps().V1().DaemonSets().Informer()
	default:
		return nil, errors.NotSupportedf("unknown deployment type")
	}
	return a.newWatcher(informer, a.name, a.clock)
}

func (a *app) WatchReplicas() (watcher.NotifyWatcher, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, 0,
		informers.WithNamespace(a.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = a.labelSelector()
		}),
	)
	return a.newWatcher(factory.Core().V1().Pods().Informer(), a.name, a.clock)
}

func (a *app) State() (caas.ApplicationState, error) {
	state := caas.ApplicationState{}
	switch a.deploymentType {
	case caas.DeploymentStateful:
		ss := resources.NewStatefulSet(a.name, a.namespace, nil)
		err := ss.Get(context.Background(), a.client)
		if err != nil {
			return caas.ApplicationState{}, errors.Trace(err)
		}
		if ss.Spec.Replicas == nil {
			return caas.ApplicationState{}, errors.Errorf("missing replicas")
		}
		state.DesiredReplicas = int(*ss.Spec.Replicas)
	case caas.DeploymentStateless:
		d := resources.NewDeployment(a.name, a.namespace, nil)
		err := d.Get(context.Background(), a.client)
		if err != nil {
			return caas.ApplicationState{}, errors.Trace(err)
		}
		if d.Spec.Replicas == nil {
			return caas.ApplicationState{}, errors.Errorf("missing replicas")
		}
		state.DesiredReplicas = int(*d.Spec.Replicas)
	case caas.DeploymentDaemon:
		d := resources.NewDaemonSet(a.name, a.namespace, nil)
		err := d.Get(context.Background(), a.client)
		if err != nil {
			return caas.ApplicationState{}, errors.Trace(err)
		}
		state.DesiredReplicas = int(d.Status.DesiredNumberScheduled)
	default:
		return caas.ApplicationState{}, errors.NotSupportedf("unknown deployment type")
	}
	next := ""
	for {
		res, err := a.client.CoreV1().Pods(a.namespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: a.labelSelector(),
			Continue:      next,
		})
		if err != nil {
			return caas.ApplicationState{}, errors.Trace(err)
		}
		for _, pod := range res.Items {
			state.Replicas = append(state.Replicas, pod.Name)
		}
		if res.RemainingItemCount == nil || *res.RemainingItemCount == 0 {
			break
		}
		next = res.Continue
	}
	sort.Strings(state.Replicas)
	return state, nil
}

// Units of the application fetched from kubernetes by matching pod labels.
func (a *app) Units() ([]caas.Unit, error) {
	ctx := context.Background()
	now := a.clock.Now()
	var units []caas.Unit
	pods, err := resources.ListPods(ctx, a.client, a.namespace, metav1.ListOptions{
		LabelSelector: a.labelSelector(),
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, p := range pods {
		var ports []string
		for _, c := range p.Spec.Containers {
			for _, p := range c.Ports {
				ports = append(ports, fmt.Sprintf("%v/%v", p.ContainerPort, p.Protocol))
			}
		}
		terminated := p.DeletionTimestamp != nil
		statusMessage, unitStatus, since, err := p.ComputeStatus(ctx, a.client, now)
		if err != nil {
			return nil, errors.Trace(err)
		}
		unitInfo := caas.Unit{
			Id:       p.Name,
			Address:  p.Status.PodIP,
			Ports:    ports,
			Dying:    terminated,
			Stateful: a.deploymentType == caas.DeploymentStateful,
			Status: status.StatusInfo{
				Status:  unitStatus,
				Message: statusMessage,
				Since:   &since,
			},
		}

		volumesByName := make(map[string]corev1.Volume)
		for _, pv := range p.Spec.Volumes {
			volumesByName[pv.Name] = pv
		}

		// Gather info about how filesystems are attached/mounted to the pod.
		// The mount name represents the filesystem tag name used by Juju.
		for _, volMount := range p.Spec.Containers[0].VolumeMounts {
			if volMount.Name == charmVolumeName {
				continue
			}
			vol, ok := volumesByName[volMount.Name]
			if !ok {
				logger.Warningf("volume for volume mount %q not found", volMount.Name)
				continue
			}
			fsInfo, err := storage.FilesystemInfo(ctx, a.client, a.namespace, vol, volMount, now)
			if err != nil {
				return nil, errors.Annotatef(err, "finding filesystem info for %v", volMount.Name)
			}
			if fsInfo == nil {
				continue
			}
			if fsInfo.StorageName == "" {
				if valid := constants.LegacyPVNameRegexp.MatchString(volMount.Name); valid {
					fsInfo.StorageName = constants.LegacyPVNameRegexp.ReplaceAllString(volMount.Name, "$storageName")
				} else if valid := constants.PVNameRegexp.MatchString(volMount.Name); valid {
					fsInfo.StorageName = constants.PVNameRegexp.ReplaceAllString(volMount.Name, "$storageName")
				}
			}
			logger.Debugf("filesystem info for %v: %+v", volMount.Name, *fsInfo)
			unitInfo.FilesystemInfo = append(unitInfo.FilesystemInfo, *fsInfo)
		}
		units = append(units, unitInfo)
	}
	return units, nil
}

// applicationPodSpec returns a PodSpec for the application pod
// of the specified application.
func (a *app) applicationPodSpec(config caas.ApplicationConfig) (*corev1.PodSpec, error) {
	appSecret := a.secretName()

	jujuDataDir := paths.DataDir(paths.OSUnixLike)

	containerNames := []string(nil)
	containers := []caas.ContainerConfig(nil)
	for _, v := range config.Containers {
		containerNames = append(containerNames, v.Name)
		containers = append(containers, v)
	}
	sort.Strings(containerNames)
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Name < containers[j].Name
	})

	resourceRequests := corev1.ResourceList(nil)
	millicores := 0
	// TODO(allow resource limits to be applied to each container).
	// For now we only do resource requests, one container is sufficient for
	// scheduling purposes.
	if config.Constraints.HasCpuPower() {
		if resourceRequests == nil {
			resourceRequests = corev1.ResourceList{}
		}
		// 100 cpu power is 100 millicores.
		millicores = int(*config.Constraints.CpuPower)
		resourceRequests[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(millicores), resource.DecimalSI)
	}
	if config.Constraints.HasMem() {
		if resourceRequests == nil {
			resourceRequests = corev1.ResourceList{}
		}
		bytes := *config.Constraints.Mem * 1024 * 1024
		resourceRequests[corev1.ResourceMemory] = *resource.NewQuantity(int64(bytes), resource.BinarySI)
	}

	containerSpecs := []corev1.Container{{
		Name:            unitContainerName,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Image:           config.CharmBaseImage.RegistryPath,
		WorkingDir:      jujuDataDir,
		Command:         []string{"/charm/bin/containeragent"},
		Args: []string{
			"unit",
			"--data-dir", jujuDataDir,
			"--charm-modified-version", strconv.Itoa(config.CharmModifiedVersion),
			"--append-env", "PATH=$PATH:/charm/bin",
		},
		Env: []corev1.EnvVar{
			{
				Name:  "JUJU_CONTAINER_NAMES",
				Value: strings.Join(containerNames, ","),
			},
			{
				Name:  constants.EnvAgentHTTPProbePort,
				Value: constants.AgentHTTPProbePort,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  int64Ptr(0),
			RunAsGroup: int64Ptr(0),
		},
		LivenessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: constants.AgentHTTPPathLiveness,
					Port: intstr.Parse(constants.AgentHTTPProbePort),
				},
			},
			InitialDelaySeconds: agentProbeInitialDelay,
			PeriodSeconds:       agentProbePeriod,
			SuccessThreshold:    agentProbeSuccess,
			FailureThreshold:    agentProbeFailure,
		},
		ReadinessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: constants.AgentHTTPPathReadiness,
					Port: intstr.Parse(constants.AgentHTTPProbePort),
				},
			},
			InitialDelaySeconds: agentProbeInitialDelay,
			PeriodSeconds:       agentProbePeriod,
			SuccessThreshold:    agentProbeSuccess,
			FailureThreshold:    agentProbeFailure,
		},
		StartupProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: constants.AgentHTTPPathStartup,
					Port: intstr.Parse(constants.AgentHTTPProbePort),
				},
			},
			InitialDelaySeconds: agentProbeInitialDelay,
			PeriodSeconds:       agentProbePeriod,
			SuccessThreshold:    agentProbeSuccess,
			FailureThreshold:    agentProbeFailure,
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      charmVolumeName,
				MountPath: "/charm/bin",
				SubPath:   "charm/bin",
				ReadOnly:  true,
			},
			{
				Name:      charmVolumeName,
				MountPath: jujuDataDir,
				SubPath:   strings.TrimPrefix(jujuDataDir, "/"),
			},
			{
				Name:      charmVolumeName,
				MountPath: "/charm/containers",
				SubPath:   "charm/containers",
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: resourceRequests,
		},
	}}

	for _, v := range containers {
		container := corev1.Container{
			Name:            v.Name,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Image:           v.Image.RegistryPath,
			Command:         []string{"/charm/bin/pebble"},
			Args: []string{
				"run",
				"--create-dirs",
				"--hold",
			},
			Env: []corev1.EnvVar{{
				Name:  "JUJU_CONTAINER_NAME",
				Value: v.Name,
			}, {
				Name:  "PEBBLE_SOCKET",
				Value: "/charm/container/pebble.socket",
			}},
			// Run Pebble as root (because it's a service manager).
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:  int64Ptr(0),
				RunAsGroup: int64Ptr(0),
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      charmVolumeName,
					MountPath: "/charm/bin/pebble",
					SubPath:   "charm/bin/pebble",
					ReadOnly:  true,
				},
				{
					Name:      charmVolumeName,
					MountPath: "/charm/container",
					SubPath:   fmt.Sprintf("charm/containers/%s", v.Name),
				},
			},
		}
		containerSpecs = append(containerSpecs, container)
	}

	nodeSelector := map[string]string(nil)
	if config.Constraints.HasArch() {
		cpuArch := *config.Constraints.Arch
		cpuArch = arch.NormaliseArch(cpuArch)
		// Convert to Golang arch string
		switch cpuArch {
		case arch.AMD64:
			cpuArch = "amd64"
		case arch.ARM64:
			cpuArch = "arm64"
		case arch.PPC64EL:
			cpuArch = "ppc64le"
		case arch.S390X:
			cpuArch = "s390x"
		default:
			return nil, errors.NotSupportedf("architecture %q", cpuArch)
		}
		nodeSelector = map[string]string{"kubernetes.io/arch": cpuArch}
	}

	automountToken := false
	return &corev1.PodSpec{
		AutomountServiceAccountToken: &automountToken,
		NodeSelector:                 nodeSelector,
		InitContainers: []corev1.Container{{
			Name:            "charm-init",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Image:           config.AgentImagePath,
			WorkingDir:      jujuDataDir,
			Command:         []string{"/opt/containeragent"},
			Args:            []string{"init", "--data-dir", jujuDataDir, "--bin-dir", "/charm/bin"},
			Env: []corev1.EnvVar{
				{
					Name:  "JUJU_CONTAINER_NAMES",
					Value: strings.Join(containerNames, ","),
				},
				{
					Name: "JUJU_K8S_POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
				{
					Name: "JUJU_K8S_POD_UUID",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.uid",
						},
					},
				},
			},
			EnvFrom: []corev1.EnvFromSource{
				{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: appSecret,
						},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      charmVolumeName,
					MountPath: jujuDataDir,
					SubPath:   strings.TrimPrefix(jujuDataDir, "/"),
				},
				{
					Name:      charmVolumeName,
					MountPath: "/charm/bin",
					SubPath:   "charm/bin",
				},
				// DO we need this in init container????
				{
					Name:      charmVolumeName,
					MountPath: "/charm/containers",
					SubPath:   "charm/containers",
				},
			},
		}},
		Containers: containerSpecs,
		Volumes: []corev1.Volume{
			{
				Name: charmVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		},
	}, nil
}

func (a *app) annotations(config caas.ApplicationConfig) annotations.Annotation {
	return k8sutils.ResourceTagsToAnnotations(config.ResourceTags, a.legacyLabels).
		Merge(k8sutils.AnnotationsForVersion(config.AgentVersion.String(), a.legacyLabels))
}

func (a *app) labels() labels.Set {
	// TODO: add modelUUID for global resources?
	return k8sutils.LabelsForApp(a.name, a.legacyLabels)
}

func (a *app) selectorLabels() labels.Set {
	return k8sutils.SelectorLabelsForApp(a.name, a.legacyLabels)
}

func (a *app) labelSelector() string {
	return k8sutils.LabelsToSelector(
		k8sutils.SelectorLabelsForApp(a.name, a.legacyLabels),
	).String()
}

func (a *app) fieldSelector() string {
	return fields.AndSelectors(
		fields.OneTermEqualSelector("metadata.name", a.name),
		fields.OneTermEqualSelector("metadata.namespace", a.namespace),
	).String()
}

func (a *app) secretName() string {
	return a.name + "-application-config"
}

type annotationGetter interface {
	GetAnnotations() map[string]string
}

func (a *app) getStorageUniqPrefix(getMeta func() (annotationGetter, error)) (string, error) {
	if r, err := getMeta(); err == nil {
		// TODO: remove this function with existing one once we consolidated the annotation keys.
		if uniqID := r.GetAnnotations()[k8sutils.AnnotationKeyApplicationUUID(false)]; len(uniqID) > 0 {
			return uniqID, nil
		}
	} else if !errors.IsNotFound(err) {
		return "", errors.Trace(err)
	}
	return a.randomPrefix()
}

type handleVolumeFunc func(vol corev1.Volume, mountPath string, readOnly bool) (*corev1.VolumeMount, error)
type handlePVCFunc func(pvc corev1.PersistentVolumeClaim, mountPath string, readOnly bool) (*corev1.VolumeMount, error)
type handleVolumeMountFunc func(string, corev1.VolumeMount) error
type handleStorageClassFunc func(storagev1.StorageClass) error

func (a *app) volumeName(storageName string) string {
	return fmt.Sprintf("%s-%s", a.name, storageName)
}

func (a *app) configureStorage(
	storageUniqueID string,
	filesystems []jujustorage.KubernetesFilesystemParams,
	storageClasses []resources.StorageClass,
	handleVolume handleVolumeFunc,
	handleVolumeMount handleVolumeMountFunc,
	handlePVC handlePVCFunc,
	handleStorageClass handleStorageClassFunc,
) error {
	storageClassMap := make(map[string]resources.StorageClass)
	for _, v := range storageClasses {
		storageClassMap[v.Name] = v
	}

	fsNames := set.NewStrings()
	for index, fs := range filesystems {
		if fsNames.Contains(fs.StorageName) {
			return errors.NotValidf("duplicated storage name %q for %q", fs.StorageName, a.name)
		}
		fsNames.Add(fs.StorageName)

		logger.Debugf("%s has filesystem %s: %s", a.name, fs.StorageName, pretty.Sprint(fs))

		readOnly := false
		if fs.Attachment != nil {
			readOnly = fs.Attachment.ReadOnly
		}

		name := a.volumeName(fs.StorageName)
		pvcNameGetter := func(volName string) string { return fmt.Sprintf("%s-%s", volName, storageUniqueID) }

		vol, pvc, sc, err := a.filesystemToVolumeInfo(name, fs, storageClassMap, pvcNameGetter)
		if err != nil {
			return errors.Trace(err)
		}

		var volumeMount *corev1.VolumeMount
		mountPath := storage.GetMountPathForFilesystem(index, a.name, fs)
		if vol != nil && handleVolume != nil {
			logger.Debugf("using volume for %s filesystem %s: %s", a.name, fs.StorageName, pretty.Sprint(*vol))
			volumeMount, err = handleVolume(*vol, mountPath, readOnly)
			if err != nil {
				return errors.Trace(err)
			}
		}
		if sc != nil && handleStorageClass != nil {
			logger.Debugf("creating storage class for %s filesystem %s: %s", a.name, fs.StorageName, pretty.Sprint(*sc))
			if err = handleStorageClass(*sc); err != nil {
				return errors.Trace(err)
			}
			storageClassMap[sc.Name] = resources.StorageClass{StorageClass: *sc}
		}
		if pvc != nil && handlePVC != nil {
			logger.Debugf("using persistent volume claim for %s filesystem %s: %s", a.name, fs.StorageName, pretty.Sprint(*pvc))
			volumeMount, err = handlePVC(*pvc, mountPath, readOnly)
			if err != nil {
				return errors.Trace(err)
			}
		}

		if volumeMount != nil {
			if err = handleVolumeMount(fs.StorageName, *volumeMount); err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}

func (a *app) filesystemToVolumeInfo(name string,
	fs jujustorage.KubernetesFilesystemParams,
	storageClasses map[string]resources.StorageClass,
	pvcNameGetter func(volName string) string,
) (*corev1.Volume, *corev1.PersistentVolumeClaim, *storagev1.StorageClass, error) {
	fsSize, err := resource.ParseQuantity(fmt.Sprintf("%dMi", fs.Size))
	if err != nil {
		return nil, nil, nil, errors.Annotatef(err, "invalid volume size %v", fs.Size)
	}

	volumeSource, err := storage.VolumeSourceForFilesystem(fs)
	if err != nil {
		return nil, nil, nil, errors.Trace(err)
	}
	if volumeSource != nil {
		vol := &corev1.Volume{
			Name:         name,
			VolumeSource: *volumeSource,
		}
		return vol, nil, nil, nil
	}

	params, err := storage.ParseVolumeParams(pvcNameGetter(name), fsSize, fs.Attributes)
	if err != nil {
		return nil, nil, nil, errors.Annotatef(err, "getting volume params for %s", fs.StorageName)
	}

	var newStorageClass *storagev1.StorageClass
	qualifiedStorageClassName := constants.QualifiedStorageClassName(a.namespace, params.StorageConfig.StorageClass)
	if _, ok := storageClasses[params.StorageConfig.StorageClass]; ok {
		// Do nothing
	} else if _, ok := storageClasses[qualifiedStorageClassName]; ok {
		params.StorageConfig.StorageClass = qualifiedStorageClassName
	} else {
		sp := storage.StorageProvisioner(a.namespace, a.modelName, *params)
		newStorageClass = storage.StorageClassSpec(sp, a.legacyLabels)
		params.StorageConfig.StorageClass = newStorageClass.Name
	}

	labels := k8sutils.LabelsMerge(
		k8sutils.LabelsForStorage(fs.StorageName, a.legacyLabels),
		k8sutils.LabelsJuju)

	pvcSpec := storage.PersistentVolumeClaimSpec(*params)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   params.Name,
			Labels: labels,
			Annotations: k8sutils.ResourceTagsToAnnotations(fs.ResourceTags, a.legacyLabels).
				Merge(k8sutils.AnnotationsForStorage(fs.StorageName, a.legacyLabels)).
				ToMap(),
		},
		Spec: *pvcSpec,
	}
	return nil, pvc, newStorageClass, nil
}

func int32Ptr(v int32) *int32 {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

func boolPtr(b bool) *bool {
	return &b
}

func strPtr(b string) *string {
	return &b
}
