#!/bin/bash


CLUSTER_NAME="${CLUSTER_NAME:-ros-e2e}"
if ! kind get clusters | grep "$CLUSTER_NAME"; then
cat << EOF > kind.config
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "ingress-ready=true"
EOF
    kind create cluster --name $CLUSTER_NAME --config kind.config
    rm -rf kind.config
fi

set -e

kubectl cluster-info --context kind-$CLUSTER_NAME
echo "Sleep to give times to node to populate with all info"
sleep 10
export EXTERNAL_IP=$(kubectl get nodes -o jsonpath='{.items[].status.addresses[?(@.type == "InternalIP")].address}')
kubectl get nodes -o wide
cd $ROOT_DIR/tests &&  ginkgo -r -v ./e2e