{{- /*
The helpers of the network policy of the setec namespace. Two engines render
it, and both read the same values (values.yaml systemPolicy):

  cilium         templates/system-namespace-policy.yaml. Cilium names a peer by
                 entity (the API server) and by host name (toFQDNs).
  networkpolicy  templates/system-namespace-networkpolicy.yaml. A plain
                 NetworkPolicy names a peer by Pod label and by CIDR only, so
                 the install states each destination as CIDRs and ports
                 (systemPolicy.networkPolicy).

docs/design/network-policy.md says what each engine enforces.
*/ -}}

{{/*
setec.systemPolicyEngine returns the engine, and fails for a name that is not
one of the two.
*/}}
{{- define "setec.systemPolicyEngine" -}}
{{- $e := .Values.systemPolicy.engine | default "cilium" -}}
{{- if not (has $e (list "cilium" "networkpolicy")) -}}
{{- fail (printf "systemPolicy.engine=%q is not valid. Use \"cilium\" (the default, for a cluster with Cilium) or \"networkpolicy\" (for a cluster whose network plugin enforces Kubernetes NetworkPolicy)." $e) -}}
{{- end -}}
{{- $e -}}
{{- end -}}

{{/*
setec.systemDNS returns, as JSON, where the Pods of the namespace resolve
names. Both engines read it. The value is systemPolicy.dns. With no namespace,
no Pod label and no CIDR, it is the Pods of CoreDNS or kube-dns in kube-system.
The old path systemPolicy.networkPolicy.dns still wins when an install sets it.
It is deprecated and the chart drops it in the next minor version.
*/}}
{{- define "setec.systemDNS" -}}
{{- $sp := .Values.systemPolicy -}}
{{- $d := $sp.dns | default dict -}}
{{- $old := dict -}}
{{- if $sp.networkPolicy -}}
{{- $old = $sp.networkPolicy.dns | default dict -}}
{{- end -}}
{{- if $old -}}
{{- $d = $old -}}
{{- end -}}
{{- $namespace := $d.namespace | default "" -}}
{{- $labels := $d.podLabels | default dict -}}
{{- $cidrs := $d.cidrs | default list -}}
{{- if or $namespace $labels -}}
{{- if not (and $namespace $labels) -}}
{{- fail "systemPolicy.dns: set both namespace and podLabels, or neither and list cidrs only" -}}
{{- end -}}
{{- else if not $cidrs -}}
{{- $namespace = "kube-system" -}}
{{- $labels = dict "k8s-app" "kube-dns" -}}
{{- end -}}
{{- range $c := $cidrs -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" (toString $c)) -}}
{{- fail (printf "systemPolicy.dns.cidrs: %q is not a CIDR. Write an address and a prefix length, such as 169.254.20.10/32." $c) -}}
{{- end -}}
{{- end -}}
{{- toJson (dict "namespace" $namespace "podLabels" $labels "cidrs" $cidrs) -}}
{{- end -}}

{{/*
setec.otlpTarget returns, as JSON, the collector that the operator exports
traces to. The value is observability.otlpEndpoint, the same value that sets
the flag --otlp-endpoint of the operator. The result is empty when tracing is
off. host and port come from a host:port address, and a scheme in front is
ignored. A port of 443 is the one that the gRPC client dials when the address
names none. isIP is true for an IPv4 or IPv6 address, and cidr is that address
as a CIDR. inCluster is true for the name of a Service of the cluster
(<service>.<namespace>.svc).
*/}}
{{- define "setec.otlpTarget" -}}
{{- $o := .Values.observability -}}
{{- if and $o.enabled $o.otlpEndpoint -}}
{{- $raw := toString $o.otlpEndpoint -}}
{{- if contains "://" $raw -}}
{{- $raw = (urlParse $raw).host -}}
{{- end -}}
{{- $raw = first (splitList "/" $raw) -}}
{{- $host := "" -}}
{{- $port := "443" -}}
{{- if hasPrefix "[" $raw -}}
{{- $host = trimPrefix "[" (first (splitList "]" $raw)) -}}
{{- $rest := trimPrefix ":" (last (splitList "]" $raw)) -}}
{{- if $rest -}}{{- $port = $rest -}}{{- end -}}
{{- else -}}
{{- $parts := splitList ":" $raw -}}
{{- $host = first $parts -}}
{{- if gt (len $parts) 1 -}}{{- $port = last $parts -}}{{- end -}}
{{- end -}}
{{- if or (not $host) (not (regexMatch "^[0-9]+$" $port)) (lt (int $port) 1) (gt (int $port) 65535) -}}
{{- fail (printf "observability.otlpEndpoint=%q is not valid. Write host:port, such as otel-collector.monitoring.svc:4317." $o.otlpEndpoint) -}}
{{- end -}}
{{- $isV4 := regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" $host -}}
{{- $isV6 := contains ":" $host -}}
{{- toJson (dict "host" $host "port" $port "isIP" (or $isV4 $isV6) "cidr" (printf "%s/%s" $host (ternary "32" "128" $isV4)) "inCluster" (regexMatch "\\.svc(\\.|$)" $host)) -}}
{{- end -}}
{{- end -}}

{{/*
setec.validateOtlp checks that the policy can name the collector of the
traces under the engine of the install. A policy that does not name it drops
the export, and the operator logs a failed export only.
*/}}
{{- define "setec.validateOtlp" -}}
{{- $t := include "setec.otlpTarget" . | default "{}" | fromJson -}}
{{- if $t.host -}}
{{- $sp := .Values.systemPolicy -}}
{{- $peer := $sp.otlp | default dict -}}
{{- if or $peer.namespace $peer.podLabels -}}
{{- if not (and $peer.namespace $peer.podLabels) -}}
{{- fail "systemPolicy.otlp: set both namespace and podLabels, or neither" -}}
{{- end -}}
{{- end -}}
{{- $havePeer := and $peer.namespace $peer.podLabels -}}
{{- if eq (include "setec.systemPolicyEngine" .) "cilium" -}}
{{- if and $t.inCluster (not $havePeer) -}}
{{- fail (printf "observability.otlpEndpoint names a Service of the cluster (%s). A policy by host name cannot match a Service, because the network plugin sees the Pod behind it. Give the namespace and the Pod labels of the collector in systemPolicy.otlp." $t.host) -}}
{{- end -}}
{{- else -}}
{{- $e := .Values.systemPolicy.networkPolicy.egress.otlp | default dict -}}
{{- if and (not $havePeer) (not $t.isIP) (not $e.cidrs) -}}
{{- fail (printf "systemPolicy.networkPolicy.egress.otlp.cidrs is required with systemPolicy.engine=networkpolicy when observability.otlpEndpoint names a host (%s). A plain NetworkPolicy cannot match a host name, so list the CIDRs of the collector, or give the namespace and Pod labels of a collector in this cluster in systemPolicy.otlp." $t.host) -}}
{{- end -}}
{{- range $c := ($e.cidrs | default list) -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" (toString $c)) -}}
{{- fail (printf "systemPolicy.networkPolicy.egress.otlp.cidrs: %q is not a CIDR. Write an address and a prefix length, such as 192.0.2.10/32." $c) -}}
{{- end -}}
{{- end -}}
{{- if $e.ports -}}
{{- include "setec.validateNetworkPolicyPorts" (dict "name" "systemPolicy.networkPolicy.egress.otlp.ports" "ports" $e.ports) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
setec.registryAddress splits the registry of an image source or of the disk
repository into its host and port. The port is 443 when the registry names
none. isIP is true for an IPv4 address. A registry at an address needs no host
list: the policy reaches that one address and port, and nothing else.
*/}}
{{- define "setec.registryAddress" -}}
{{- $parts := splitList ":" (first (splitList "/" .)) -}}
{{- $host := first $parts -}}
{{- $port := ternary (last $parts) "443" (gt (len $parts) 1) -}}
{{- toJson (dict "host" $host "port" $port "isIP" (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" $host)) -}}
{{- end -}}

{{/*
setec.validateSystemPolicy checks the values of both engines, and the inputs
that the engine of the install needs.
*/}}
{{- define "setec.validateSystemPolicy" -}}
{{- $sp := .Values.systemPolicy -}}
{{- $engine := include "setec.systemPolicyEngine" . -}}
{{- $_ := include "setec.systemDNS" . -}}
{{- $_ := include "setec.validateOtlp" . -}}
{{- range $c := $sp.webhookCallers -}}
{{- if not (and $c.namespace $c.podLabels) -}}
{{- fail "systemPolicy.webhookCallers: each entry needs a namespace and podLabels. A Pod label alone is not proof, because the person who creates a Pod chooses its labels, so the namespace of the caller is pinned too." -}}
{{- end -}}
{{- end -}}
{{- range $c := $sp.metricsScrapers -}}
{{- if not (and $c.namespace $c.podLabels) -}}
{{- fail "systemPolicy.metricsScrapers: each entry needs a namespace and podLabels. A Pod label alone is not proof, because the person who creates a Pod chooses its labels, so the namespace of the scraper is pinned too." -}}
{{- end -}}
{{- end -}}
{{- if and .Values.frontend.enabled .Values.frontend.clients (not $sp.frontendCallers) -}}
{{- fail "systemPolicy.frontendCallers must not be empty when frontend.enabled=true: the namespace policy allows the frontend gRPC port only from these Pods. Name the namespace and the Pod labels of each caller (the Gibson daemon)." -}}
{{- end -}}
{{- range $i, $e := $sp.credentialEndpoints -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" (toString $e.cidr)) -}}
{{- fail (printf "systemPolicy.credentialEndpoints[%d].cidr: %q is not a CIDR. Write an address and a prefix length, such as 169.254.170.23/32." $i $e.cidr) -}}
{{- end -}}
{{- include "setec.validateNetworkPolicyPorts" (dict "name" (printf "systemPolicy.credentialEndpoints[%d].ports" $i) "ports" $e.ports) -}}
{{- end -}}
{{- if eq $engine "cilium" -}}
{{- /* An object of a kind that the API server does not know cannot be
applied. Stop here, and say what to do. helm install and helm upgrade read
the kinds from the cluster. helm template reads the list that --api-versions
gives. */ -}}
{{- $missing := list -}}
{{- range $kind := list "CiliumNetworkPolicy" "CiliumClusterwideNetworkPolicy" -}}
{{- if not ($.Capabilities.APIVersions.Has (printf "cilium.io/v2/%s" $kind)) -}}
{{- $missing = append $missing $kind -}}
{{- end -}}
{{- end -}}
{{- if $missing -}}
{{- fail (printf "systemPolicy.engine is \"cilium\", but the cluster has no CRD for %s (API group cilium.io/v2). Install Cilium as the network plugin, or set systemPolicy.engine=networkpolicy and the destinations of systemPolicy.networkPolicy, which render plain NetworkPolicy objects. A render with no cluster, such as helm template, needs the flags --api-versions cilium.io/v2/CiliumNetworkPolicy --api-versions cilium.io/v2/CiliumClusterwideNetworkPolicy." (join " and " $missing)) -}}
{{- end -}}
{{- else -}}
{{- include "setec.validateNetworkPolicyEngine" . -}}
{{- end -}}
{{- end -}}

{{/*
setec.validateNetworkPolicyEngine checks the destinations of the networkpolicy
engine. A plain NetworkPolicy has no host name and no API server entity, so
the install names each destination as CIDRs and ports. A port list that is
empty would allow every port, so an empty list is refused too.
*/}}
{{- define "setec.validateNetworkPolicyEngine" -}}
{{- $np := .Values.systemPolicy.networkPolicy -}}
{{- if not $np.apiServer.cidrs -}}
{{- fail "systemPolicy.networkPolicy.apiServer.cidrs is required with systemPolicy.engine=networkpolicy. A plain NetworkPolicy cannot name the API server, so list the addresses of its endpoints as CIDRs (kubectl get endpoints kubernetes -n default). The operator and the frontend reach only these addresses, on systemPolicy.networkPolicy.apiServer.ports." -}}
{{- end -}}
{{- include "setec.validateNetworkPolicyPorts" (dict "name" "systemPolicy.networkPolicy.apiServer.ports" "ports" $np.apiServer.ports) -}}
{{- range $name, $dest := dict "apiServer" $np.apiServer "egress.imageSources" $np.egress.imageSources "egress.diskRegistry" $np.egress.diskRegistry "egress.sigstore" $np.egress.sigstore "egress.snapshotStore" $np.egress.snapshotStore -}}
{{- range $c := ($dest.cidrs | default list) -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" (toString $c)) -}}
{{- fail (printf "systemPolicy.networkPolicy %s: %q is not a CIDR. Write an address and a prefix length, such as 192.0.2.10/32." $name $c) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $c := $np.webhookSourceCIDRs -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" (toString $c)) -}}
{{- fail (printf "systemPolicy.networkPolicy.webhookSourceCIDRs: %q is not a CIDR. Write an address and a prefix length, such as 192.0.2.10/32." $c) -}}
{{- end -}}
{{- end -}}
{{- with .Values.launcher.diskRepo -}}
{{- $disk := include "setec.registryAddress" . | fromJson -}}
{{- $sources := $.Values.launcher.imageSources -}}
{{- if not $sources -}}
{{- fail "launcher.imageSources must not be empty: the disk builder pulls each launcher image from one of these registries, and reaches no other host" -}}
{{- end -}}
{{- $named := false -}}
{{- range $src := $sources -}}
{{- if not $src.registry -}}
{{- fail "each launcher.imageSources entry needs a registry" -}}
{{- end -}}
{{- $addr := include "setec.registryAddress" $src.registry | fromJson -}}
{{- if not $addr.isIP -}}
{{- $named = true -}}
{{- end -}}
{{- end -}}
{{- if and $named (not $np.egress.imageSources.cidrs) -}}
{{- fail "systemPolicy.networkPolicy.egress.imageSources.cidrs is required with systemPolicy.engine=networkpolicy when a launcher.imageSources entry names a host. A plain NetworkPolicy cannot match a host name, so list the CIDRs of the registry and of the hosts that serve its blobs. The disk Jobs reach only these CIDRs, on egress.imageSources.ports." -}}
{{- end -}}
{{- include "setec.validateNetworkPolicyPorts" (dict "name" "systemPolicy.networkPolicy.egress.imageSources.ports" "ports" $np.egress.imageSources.ports) -}}
{{- if and (not $disk.isIP) (not $np.egress.diskRegistry.cidrs) -}}
{{- fail (printf "systemPolicy.networkPolicy.egress.diskRegistry.cidrs is required with systemPolicy.engine=networkpolicy when launcher.diskRepo names a host (%s). List the CIDRs of the disk registry. A disk repository at an IP address needs none." $disk.host) -}}
{{- end -}}
{{- if $np.egress.sigstore.cidrs -}}
{{- include "setec.validateNetworkPolicyPorts" (dict "name" "systemPolicy.networkPolicy.egress.sigstore.ports" "ports" $np.egress.sigstore.ports) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.snapshots.enabled .Values.snapshots.s3.enabled -}}
{{- $s3 := .Values.snapshots.s3 -}}
{{- $isIP := false -}}
{{- if $s3.endpoint -}}
{{- $isIP = regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" (first (splitList ":" (urlParse $s3.endpoint).host)) -}}
{{- end -}}
{{- if and (not $isIP) (not $np.egress.snapshotStore.cidrs) -}}
{{- fail "systemPolicy.networkPolicy.egress.snapshotStore.cidrs is required with systemPolicy.engine=networkpolicy when snapshots.s3.enabled=true and the store is not at an IP address. List the CIDRs of the S3 endpoint. The node agent reaches only these CIDRs." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
setec.validateNetworkPolicyPorts refuses an empty list of ports, because a
rule with no port allows every port. Call it with (dict "name" <value path>
"ports" <list>).
*/}}
{{- define "setec.validateNetworkPolicyPorts" -}}
{{- if not .ports -}}
{{- fail (printf "%s must list at least one port: a rule with no port allows every port" .name) -}}
{{- end -}}
{{- range $p := .ports -}}
{{- if or (lt (int $p) 1) (gt (int $p) 65535) -}}
{{- fail (printf "%s: %v is not a port number" $.name $p) -}}
{{- end -}}
{{- end -}}
{{- end -}}
