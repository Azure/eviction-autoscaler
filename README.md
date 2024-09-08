# Project Name

[![Go Report Card](https://goreportcard.com/badge/github.com/paulgmiller/k8s-pdb-autoscaler)](https://goreportcard.com/report/github.com/paulgmiller/k8s-pdb-autoscaler)
[![GoDoc](https://pkg.go.dev/badge/github.com/paulgmiller/k8s-pdb-autoscaler)](https://pkg.go.dev/github.com/paulgmiller/k8s-pdb-autoscaler)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)


## Table of Contents

- [Introduction](#introduction)
- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)

## Introduction

This originated as an intern project still at github.com/Javier090/k8s-pdb-autoscaler

The general idea is that k8s deployments already have a max surge concept and there's no reason that surge is only for new deployments and not for node maitence.
It captures evictions using a webhook and writes them to a PDBWatcher CR if it exists. A controller will then try and temporarily scale up the deployment that corresponds.

### Why 
Overprovisioning isn't free. Sometimes it makes sense to run as cheap as you can. But you still don't want to be down because there was a cluster upgrade or even a vm maintence event.
Your app might also just be having a bad time for unrelated reasons and an the same maitence event shouldn't cost you down time if extra replicas can save you.

## Features

- Web hook that writes evictions to pdb watcher custom resource.
- Controller that wathces pdb watchers and if evictions are blocked because watchers PDB's disruptionsAllowed is zero then surge deployment.
- Controller Restores deployment when evictions go through with 


```mermaid
graph TD;
    Eviction[Eviction]
    Webhook[Admission Webhook]
    CRD[Custom Resource Definition]
    Controller[Kubernetes Controller]
    Deployment[Deployment or StatefulSet]

    Eviction -->|Triggers| Webhook
    Webhook -->|writes spec| CRD 
    CRD -->|spec watched by| Controller
    Controller -->|surges and shrinks| Deployment
    Controller -->|Writes status| CRD
```

## Installation

### Prerequisites

- Docker
- kind for e2e tests.
- A sense of adventure

### Install

Clone the repository and install the dependencies:

```bash
git clone https://github.com/paulgmiller/k8s-pdb-autoscaler.git
cd k8s-pdb-autoscaler
hack/install.sh
```

## Usage
Here's how to see how this might work.

```bash
kubectl create ns laboratory
kubectl create deployment -n laboratory piggie --image nginx
hack/autodeploy.sh laboratory #want to replace this with tagging namespaces.
# show a starting state
k get pods -n laboratory
k get pdbwatcher piggie-pdb-watcher -n laboratory -o yaml
go run ./cmd/evict --label app=piggie -ns laboratory
# show we've scaled up
k get pods -n laboratory
k get pdbwatcher piggie-pdb-watcher -n laboratory -o yaml
# okay one more eviction to get us back down to one replica
go run ./cmd/evict --label app=piggie -ns laboratory
```



## TODO 

###  Tactical 
- Make the webhook cheaper. 
- Stateful set scaledown. 
- e2e
- kustomize
- cert manager. 


- Add these sections to the readme
  
  - [Configuration](#configuration)
  - [Examples](#examples)
  - [Contributing](#contributing)

