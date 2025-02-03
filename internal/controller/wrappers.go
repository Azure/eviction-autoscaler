package controllers

import (
	"fmt"

	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Surger interface {
	//GetGeneration() int64
	GetReplicas() int32
	SetReplicas(int32)
	GetMaxSurge() intstr.IntOrString
	Obj() client.Object
	//Update(ctx context.Context, obj Object, opts ...UpdateOption) error
	AddAnnotation(string, string)
	RemoveAnnotation(string)
}

// Todo change casing to match k8s?
const (
	deploymentKind  = "deployment"
	statefulSetKind = "statefulset"
)

type DeploymentWrapper struct {
	obj *v1.Deployment
}

var _ Surger = &DeploymentWrapper{}

func (d *DeploymentWrapper) Obj() client.Object {
	return d.obj
}

func (d *DeploymentWrapper) GetReplicas() int32 {
	if d.obj.Spec.Replicas == nil {
		return 1 // Default value in Kubernetes if not set
	}
	return *d.obj.Spec.Replicas
}

func (d *DeploymentWrapper) SetReplicas(replicas int32) {
	d.obj = d.obj.DeepCopy() //don't mutate the cache
	d.obj.Spec.Replicas = &replicas
}

func (d *DeploymentWrapper) GetMaxSurge() intstr.IntOrString {
	if d.obj.Spec.Strategy.RollingUpdate != nil && d.obj.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		return *d.obj.Spec.Strategy.RollingUpdate.MaxSurge
	}
	return intstr.FromInt(0)
}

// AddAnnotation  add new status annotation
func (d *DeploymentWrapper) AddAnnotation(status, newReplicas string) {
	if d.obj.Annotations == nil {
		d.obj.Annotations = make(map[string]string)
	}
	d.obj.Annotations[status] = newReplicas
}

// RemoveAnnotation will delete specific status annotation
func (d *DeploymentWrapper) RemoveAnnotation(status string) {
	//always need to new map to reset the status of deployment and clear previous ones
	if d.obj.Annotations != nil {
		delete(d.obj.Annotations, status)
	}
}

type StatefulSetWrapper struct {
	obj *v1.StatefulSet
}

var _ Surger = &StatefulSetWrapper{}

func (s *StatefulSetWrapper) Obj() client.Object {
	return s.obj
}

func (s *StatefulSetWrapper) GetReplicas() int32 {
	if s.obj.Spec.Replicas == nil {
		return 1 // Default value in Kubernetes if not set
	}
	return *s.obj.Spec.Replicas
}

func (s *StatefulSetWrapper) SetReplicas(replicas int32) {
	s.obj = s.obj.DeepCopy() //don't mutate the cache
	s.obj.Spec.Replicas = &replicas
}

func (s *StatefulSetWrapper) GetMaxSurge() intstr.IntOrString {
	return intstr.FromString("10%") //there is no max surge for stateful sets.
}

func GetSurger(kind string) (Surger, error) {
	if kind == deploymentKind {
		return &DeploymentWrapper{obj: &v1.Deployment{}}, nil
	} else if kind == statefulSetKind {
		return &StatefulSetWrapper{obj: &v1.StatefulSet{}}, nil
	} else {
		return nil, fmt.Errorf("unknown target kind %s", kind) //be good to enforce this with admission policy
	}

}

// AddAnnotation will reset and add new annotation map every time this func is called
func (s *StatefulSetWrapper) AddAnnotation(status, newReplicas string) {
	if s.obj.Annotations == nil {
		s.obj.Annotations = make(map[string]string)
	}
	s.obj.Annotations[status] = newReplicas
}

// RemoveAnnotation will delete specific status annotation
func (s *StatefulSetWrapper) RemoveAnnotation(status string) {
	//always need to new map to reset the status of deployment and clear previous ones
	if s.obj.Annotations != nil {
		delete(s.obj.Annotations, status)
	}
}
