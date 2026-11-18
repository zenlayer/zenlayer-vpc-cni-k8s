{{- /*
The apiserver endpoint for KUBERNETES_SERVICE_HOST/PORT, as YAML; empty means
the in-cluster Service. With Cilium replacing kube-proxy that Service is served
only once Cilium is up, which on a dual-stack node waits for the IPv6 this
daemon's registration brings -- so with cilium.enabled and no explicit host it
is read from kube-public/cluster-info at install / upgrade. Not found, it fails
the render rather than fall back: the fallback is the address that is served
by nothing, and an offline render (helm template, a client-side dry run, a
GitOps tool) never finds it.
*/}}
{{- define "zenlayer-vpc-cni.apiserver" -}}
{{- $host := .Values.zec.k8sServiceHost | default "" | toString -}}
{{- $port := .Values.zec.k8sServicePort | default "" | toString -}}
{{- if and (not $host) .Values.cilium.enabled -}}
{{- $cm := lookup "v1" "ConfigMap" "kube-public" "cluster-info" -}}
{{- $server := regexFind "server:[ \t]*[^ \t\r\n]+" (dig "data" "kubeconfig" "" $cm) | trimPrefix "server:" | trim -}}
{{- if $server -}}
{{- $hostPort := (urlParse $server).host -}}
{{- $p := regexFind ":[0-9]+$" $hostPort -}}
{{- $host = trimSuffix $p $hostPort | trimPrefix "[" | trimSuffix "]" -}}
{{- if not $port -}}
{{- $port = trimPrefix ":" $p | default "443" -}}
{{- end -}}
{{- else if not (lookup "v1" "Namespace" "" "kube-system") -}}
{{- fail "cilium.enabled=true needs the apiserver entry point, and an offline render (helm template, a client-side dry run, a GitOps tool) cannot read it from kube-public/cluster-info: set zec.k8sServiceHost and zec.k8sServicePort to the apiserver entry point (a load balancer or VIP)." -}}
{{- else -}}
{{- fail "cilium.enabled=true needs the apiserver entry point, and kube-public/cluster-info does not have it (not a kubeadm cluster?): set zec.k8sServiceHost and zec.k8sServicePort to the apiserver entry point (a load balancer or VIP)." -}}
{{- end -}}
{{- end -}}
host: {{ $host | quote }}
port: {{ $port | quote }}
{{- end -}}
