#!/usr/bin/env bash
# Read-only inventory. Does not fetch Secrets, credentials, or full kubeconfig.
set -u
section() { printf '\n===== %s =====\n' "$1"; }
kc() { kubectl --request-timeout=15s "$@"; }
section 'Current context'
kc config current-context
section 'Deployments: locate operator/API and their actual image/ServiceAccount'
kc get deployments -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SA:.spec.template.spec.serviceAccountName,IMAGE:.spec.template.spec.containers[*].image,READY:.status.readyReplicas,NODE_SELECTOR:.spec.template.spec.nodeSelector'
section 'Workspace locations (names only; no environment values)'
kc get workspaces -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase'
section 'Node addresses and allocated Pod CIDRs (not a complete pool inventory)'
kc get nodes -o 'custom-columns=NAME:.metadata.name,ADDRESSES:.status.addresses[*].address,POD_CIDRS:.spec.podCIDRs,RUNTIME:.status.nodeInfo.containerRuntimeVersion'
section 'Cluster DNS Service'
kc -n kube-system get service kube-dns -o wide
section 'NodeLocal DNS, if installed'
kc -n kube-system get daemonset node-local-dns -o 'custom-columns=NAME:.metadata.name,ARGS:.spec.template.spec.containers[*].args'
section 'Calico pools, if this CNI and resource are installed'
kc get ippools.crd.projectcalico.org -o 'custom-columns=NAME:.metadata.name,CIDR:.spec.cidr,DISABLED:.spec.disabled'
section 'NetworkPolicy names/selectors'
kc get networkpolicies -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SELECTOR:.spec.podSelector'
section 'Known egress ConfigMaps in default operator namespace (names only)'
kc -n agent-deploy-system get configmap workspace-egress-config workspace-egress-history workspace-egress-boundary --ignore-not-found -o name
section 'Notes'
printf '%s\n' 'NotFound for optional NodeLocal DNS/Calico resources may be expected; Forbidden or timeout means unknown, not absent.' 'Service CIDR and full management/VIP ranges require the cluster administrator to confirm. Do not infer them from individual Service IPs or node Pod CIDRs.'
