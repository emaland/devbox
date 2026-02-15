provider "aws" {
  region = "us-east-2"
}

# ── Shared config ───────────────────────────────────────────────────
# Read the same config file the devbox CLI uses so names, types, AZs,
# etc. stay in sync between Terraform and the CLI.

locals {
  devbox = jsondecode(file(pathexpand("~/.config/devbox/default.json")))
}

# ── Variables that aren't in the devbox config ──────────────────────

variable "dns_zone_id" {
  description = "Route 53 hosted zone ID for the DNS record"
  type        = string
}

variable "ssh_public_key" {
  description = "SSH public key to authorize for the emaland user"
  type        = string
}

# ── Key pair ────────────────────────────────────────────────────────

resource "aws_key_pair" "dev" {
  key_name   = local.devbox.ssh_key_name
  public_key = var.ssh_public_key
}

# ── Security group ──────────────────────────────────────────────────

data "aws_vpc" "default" {
  default = true
}

resource "aws_security_group" "dev_instance" {
  name        = local.devbox.security_group
  description = "SSH + Tailscale"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port   = 41641
    to_port     = 41641
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${local.devbox.security_group}-sg"
  }
}

# ── IAM ─────────────────────────────────────────────────────────────

resource "aws_iam_role" "dev" {
  name = "dev-workstation-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })
}

resource "aws_iam_role_policy" "route53_update" {
  name = "route53-update"
  role = aws_iam_role.dev.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "route53:ChangeResourceRecordSets"
      Resource = "arn:aws:route53:::hostedzone/${var.dns_zone_id}"
    }]
  })
}

resource "aws_iam_instance_profile" "dev" {
  name = local.devbox.iam_profile
  role = aws_iam_role.dev.name
}

# ── EBS data volume ─────────────────────────────────────────────────

resource "aws_ebs_volume" "data" {
  availability_zone = local.devbox.default_az
  size              = 512
  type              = "gp3"
  iops              = 3000
  throughput        = 250

  tags = {
    Name = "dev-data-volume"
  }

  lifecycle {
    prevent_destroy = true
  }
}
