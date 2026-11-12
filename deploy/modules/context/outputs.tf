output "cloud" {
  description = "Target cloud identifier."
  value       = var.cloud
}

output "cluster_name" {
  description = "Logical Kodama cluster name."
  value       = var.cluster_name
}

output "name_prefix" {
  description = "Standardized resource name prefix (kodama-<cluster_name>)."
  value       = local.name_prefix
}

output "installation_id" {
  description = "Deterministic Kodama installation identifier."
  value       = local.installation_id
}

output "environment" {
  description = "Kodama installation environment."
  value       = var.environment
}

output "cost_profile" {
  description = "Selected sizing profile."
  value       = var.cost_profile
}

output "is_ha" {
  description = "Whether high-availability sizing is enabled."
  value       = local.is_ha
}

output "account_or_project_id" {
  description = "Cloud account or project ID."
  value       = var.account_or_project_id
}

output "region" {
  description = "Primary cloud region."
  value       = var.region
}

output "zone" {
  description = "Primary cloud zone."
  value       = var.zone
}

output "release_namespace" {
  description = "Control-plane Kubernetes release namespace."
  value       = var.release_namespace
}

output "tenants" {
  description = "Declared tenant list."
  value       = var.tenants
}

output "tenant_namespaces" {
  description = "Map of tenant slug to Kubernetes namespace."
  value       = local.tenant_namespaces
}

output "key_adapter" {
  description = "Kodama key adapter name for the cloud provider."
  value       = local.key_adapter
}

output "key_ref_prefix" {
  description = "Kodama key reference prefix for the cloud provider."
  value       = local.key_ref_prefix
}

output "state_key" {
  description = "Per-cluster remote state object key."
  value       = local.state_key
}

output "common_labels" {
  description = "Normalized map of labels/tags for cloud resources."
  value       = local.common_labels
}

output "common_tag_list" {
  description = "Normalized list of key:value tags for providers that use tag lists (such as Scaleway)."
  value       = local.common_tag_list
}
