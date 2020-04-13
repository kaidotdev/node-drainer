# NodeDrainer

NodeDrainer is Kubernetes Component that drain a node the specified event occurred.

## Installation

```shell
$ kubectl apply -k manifests
```

## Usage

```shell
$ cat <<EOS | kubectl apply -f -
apiVersion: v1
kind: Event
metadata:
  name: test
involvedObject:
  kind: Node
  name: minikube
type: Normal
message: "Node test status is now: ContainerGCFailed"
reason: ContainerGCFailed
EOS

$ kubectl logs -l app=node-drainer
WARNING: deleting Pods not managed by ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet: kube-system/storage-provisioner
evicting pod "coredns-5644d7b6d9-lpt8b"
evicting pod "storage-provisioner"
evicting pod "node-drainer-645b59c6b9-6j8kk"
evicting pod "registry-creds-n6rsk"
evicting pod "coredns-5644d7b6d9-cpk2w"
```

The events to be detected can be configured from [--target-events](https://github.com/kaidotdev/node-drainer/blob/v0.1.0/manifests/deployment.yaml#L45)

## How to develop

### `skaffold dev`

```sh
$ make dev
```

### Test

```sh
$ make test
```

### Lint

```sh
$ make lint
```
