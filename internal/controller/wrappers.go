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
	client.Object
	//Update(ctx context.Context, obj Object, opts ...UpdateOption) error
}

const (
	deploymentKind  = "deployment"
	statefulSetKind = "deployment"
)

type DeploymentWrapper struct {
	*v1.Deployment
}

var _ Surger = &DeploymentWrapper{}

func (d *DeploymentWrapper) GetReplicas() int32 {
	if d.Spec.Replicas == nil {
		return 1 // Default value in Kubernetes if not set
	}
	return *d.Spec.Replicas
}

func (d *DeploymentWrapper) SetReplicas(replicas int32) {
	d.Spec.Replicas = &replicas
}

func (d *DeploymentWrapper) GetMaxSurge() intstr.IntOrString {
	if d.Spec.Strategy.RollingUpdate != nil && d.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		return *d.Spec.Strategy.RollingUpdate.MaxSurge
	}
	return intstr.FromInt(0)
}

type StatefulSetWrapper struct {
	*v1.StatefulSet
}

var _ Surger = &StatefulSetWrapper{}

func (s *StatefulSetWrapper) GetReplicas() int32 {
	if s.Spec.Replicas == nil {
		return 1 // Default value in Kubernetes if not set
	}
	return *s.Spec.Replicas
}

func (s *StatefulSetWrapper) SetReplicas(replicas int32) {
	s.Spec.Replicas = &replicas
}

func (s *StatefulSetWrapper) GetMaxSurge() intstr.IntOrString {
	return intstr.FromString("10%") //there is no max surge for stateful sets.
}

func GetSurger(kind string) (Surger, error) {
	if kind == deploymentKind {
		return &DeploymentWrapper{}, nil
	} else if kind == statefulSetKind {
		return &StatefulSetWrapper{}, nil
	} else {
		return nil, fmt.Errorf("unknown target kind")
	}

}
