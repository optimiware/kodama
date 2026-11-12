resource "scaleway_k8s_cluster" "main" {
  name                        = "${module.context.name_prefix}-k8s"
  region                      = var.region
  project_id                  = var.project_id
  type                        = "kapsule"
  version                     = var.kubernetes_version
  cni                         = "cilium"
  private_network_id          = scaleway_vpc_private_network.main.id
  delete_additional_resources = false
  tags                        = module.context.common_tag_list

  auto_upgrade {
    enable                        = true
    maintenance_window_start_hour = 3
    maintenance_window_day        = "sunday"
  }

  autoscaler_config {
    disable_scale_down              = false
    scale_down_delay_after_add      = "10m"
    scale_down_unneeded_time        = "10m"
    estimator                       = "binpacking"
    expander                        = "random"
    ignore_daemonsets_utilization   = true
    balance_similar_node_groups     = true
    expendable_pods_priority_cutoff = -5
  }
}

resource "scaleway_k8s_pool" "workers" {
  cluster_id             = scaleway_k8s_cluster.main.id
  name                   = "${module.context.name_prefix}-workers"
  zone                   = local.effective_zone
  node_type              = var.k8s_node_type
  size                   = var.k8s_pool_size
  min_size               = var.k8s_pool_min_size
  max_size               = var.k8s_pool_max_size
  autoscaling            = var.k8s_autoscaling
  autohealing            = true
  container_runtime      = "containerd"
  root_volume_type       = "sbs_5k"
  root_volume_size_in_gb = var.k8s_root_volume_size_gb
  tags                   = module.context.common_tag_list
}

# Only the listed operator networks and Scaleway's own ranges (the nodes and
# managed services) can connect to the Kubernetes API; authentication is still
# required.
resource "scaleway_k8s_acl" "api" {
  cluster_id = scaleway_k8s_cluster.main.id
  region     = var.region

  dynamic "acl_rules" {
    for_each = var.k8s_api_allowed_cidrs
    content {
      ip          = acl_rules.value
      description = "operator"
    }
  }

  acl_rules {
    scaleway_ranges = true
    description     = "scaleway nodes and services"
  }
}
