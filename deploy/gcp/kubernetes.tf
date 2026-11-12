resource "google_container_cluster" "main" {
  name     = "${module.context.name_prefix}-gke"
  project  = var.project_id
  location = module.context.is_ha ? var.region : local.effective_zone

  network    = google_compute_network.main.id
  subnetwork = google_compute_subnetwork.main.id

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = var.gke_deletion_protection

  # Dataplane V2 (Cilium) provides native Kubernetes NetworkPolicy enforcement.
  datapath_provider = "ADVANCED_DATAPATH"

  release_channel {
    channel = var.gke_release_channel
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  master_authorized_networks_config {
    dynamic "cidr_blocks" {
      for_each = var.k8s_api_allowed_cidrs
      content {
        cidr_block   = cidr_blocks.value
        display_name = "operator"
      }
    }
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  resource_labels = module.context.common_labels

  depends_on = [google_project_service.required]
}

resource "google_container_node_pool" "workers" {
  name               = "${module.context.name_prefix}-workers"
  project            = var.project_id
  location           = google_container_cluster.main.location
  cluster            = google_container_cluster.main.name
  initial_node_count = var.gke_node_count

  autoscaling {
    min_node_count = var.gke_min_node_count
    max_node_count = var.gke_max_node_count
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type = var.gke_machine_type
    disk_size_gb = var.gke_disk_size_gb
    disk_type    = "pd-balanced"

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    labels = module.context.common_labels
  }
}
