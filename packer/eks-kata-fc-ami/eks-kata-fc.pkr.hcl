# eks-kata-fc.pkr.hcl — Packer build for the Setec kata-fc x86-metal EKS
# node AMI (ADR-0001: the sandbox substrate is x86 only).
#
# Produces an x86_64 AMI from the current EKS-optimized AL2023 base that boots
# READY to run kata-fc Firecracker microVMs with zero runtime config
# mutation — no kata-deploy DaemonSet, no live containerd rewrites:
#
#   - kata-containers static release (includes the Firecracker VMM) under
#     /opt/kata, pinned by version (and optionally by sha256).
#   - containerd statically configured via a drop-in in
#     /etc/containerd/config.d/: kata-fc runtime handler + devmapper
#     snapshotter.
#   - a boot-time systemd unit (setec-thinpool.service) that provisions the
#     devmapper thin-pool from the instance's LOCAL NVMe instance store,
#     idempotent across reboots, ordered Before=containerd.service.
#   - a static kata-fc RuntimeClass manifest baked at
#     /etc/setec/manifests/runtimeclass-kata-fc.yaml (apply once per
#     cluster; see README.md).
#
# The build instance does NOT need KVM or bare metal — baking only writes
# files. Target instance types at runtime are the cheapest x86 metal with
# local NVMe: c6id.metal / m6id.metal (the chart's Karpenter NodePool
# defaults).
#
# Usage:
#   packer init .
#   packer build -var 'region=us-east-1' .
#
# See README.md in this directory for variables, verification steps, and the
# node-launch checklist.

packer {
  required_version = ">= 1.9.0"

  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region to build the AMI in."
}

variable "k8s_version" {
  type        = string
  default     = "1.33"
  description = "EKS Kubernetes minor version. Selects the EKS-optimized AL2023 x86_64 base AMI via the public SSM parameter."
}

variable "kata_version" {
  type        = string
  default     = "3.32.0"
  description = "Pinned kata-containers release. The static tarball bundles the Firecracker VMM, guest kernel, and rootfs images."
}

variable "kata_sha256" {
  type        = string
  default     = "1449ecea50bd91fa73a94648db195d18950fe869ba4b1f12d05f55f1fa7c1b01"
  description = "Pinned sha256 of kata-static-<kata_version>-amd64.tar.zst. REQUIRED — kata >= 3.28.0 publishes no .sha256sum sidecars, so the bake fails without a pin. Keep in lockstep with the Dockerfile.installer KATA_SHA256 pin so the AMI and the installer DaemonSet lay down the same payload. Bumping kata_version requires updating this pin."
}

variable "build_instance_type" {
  type        = string
  default     = "m7i.xlarge"
  description = "x86_64 instance type used for the bake. Baking only writes files, so no KVM/metal is required here."
}

variable "ami_name_prefix" {
  type        = string
  default     = "setec-kata-fc"
  description = "Prefix for the output AMI name. Karpenter/node-group AMI selectors should match 'setec-kata-fc-*'."
}

variable "root_volume_size_gb" {
  type        = number
  default     = 100
  description = "Root EBS volume size in GiB. Container images pulled via the default (overlayfs) snapshotter and kata guest images live here; the devmapper pool lives on instance-store NVMe."
}

# Current EKS-optimized AL2023 x86_64 AMI for the pinned Kubernetes version.
data "amazon-parameterstore" "eks_al2023_x86_64" {
  name   = "/aws/service/eks/optimized-ami/${var.k8s_version}/amazon-linux-2023/x86_64/standard/recommended/image_id"
  region = var.region
}

locals {
  timestamp = regex_replace(timestamp(), "[- TZ:]", "")
  ami_name  = "${var.ami_name_prefix}-k8s${var.k8s_version}-kata${var.kata_version}-${local.timestamp}"
}

source "amazon-ebs" "eks_kata_fc" {
  region        = var.region
  instance_type = var.build_instance_type
  source_ami    = data.amazon-parameterstore.eks_al2023_x86_64.value
  ssh_username  = "ec2-user"

  ami_name        = local.ami_name
  ami_description = "EKS AL2023 x86_64 node AMI with baked kata-containers ${var.kata_version} + Firecracker (kata-fc), static containerd config, and NVMe devmapper thin-pool boot unit. Built by zeroroot-ai/setec packer/eks-kata-fc-ami."

  # IMDSv2 only, matching the EKS-optimized base.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = var.root_volume_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name                                 = local.ami_name
    "setec.zeroroot.ai/runtime.kata-fc"  = "true"
    "setec.zeroroot.ai/kata-version"     = var.kata_version
    "setec.zeroroot.ai/k8s-version"      = var.k8s_version
    "setec.zeroroot.ai/managed-by"       = "packer/eks-kata-fc-ami"
  }

  snapshot_tags = {
    Name = local.ami_name
  }
}

build {
  sources = ["source.amazon-ebs.eks_kata_fc"]

  # Boot-time units, containerd systemd drop-in, and the static
  # RuntimeClass manifest. The destination directory must exist BEFORE the
  # file provisioner runs — uploading "files/" onto a non-existent path
  # creates a plain file, not a directory.
  provisioner "shell" {
    inline = ["mkdir -p /tmp/setec-files"]
  }

  provisioner "file" {
    source      = "files/"
    destination = "/tmp/setec-files"
  }

  provisioner "shell" {
    execute_command = "{{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "KATA_VERSION=${var.kata_version}",
      "KATA_SHA256=${var.kata_sha256}",
    ]
    script = "scripts/install-kata.sh"
  }

  provisioner "shell" {
    execute_command = "{{ .Vars }} sudo -E bash '{{ .Path }}'"
    script          = "scripts/configure-containerd.sh"
  }

  provisioner "shell" {
    execute_command = "{{ .Vars }} sudo -E bash '{{ .Path }}'"
    script          = "scripts/setup-boot-units.sh"
  }

  provisioner "shell" {
    execute_command = "{{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "KATA_VERSION=${var.kata_version}",
    ]
    script = "scripts/verify.sh"
  }
}
