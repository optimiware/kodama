resource "scaleway_vpc" "main" {
  name       = "${module.context.name_prefix}-vpc"
  region     = var.region
  project_id = var.project_id
  tags       = module.context.common_tag_list
}

resource "scaleway_vpc_private_network" "main" {
  name       = "${module.context.name_prefix}-pn"
  vpc_id     = scaleway_vpc.main.id
  region     = var.region
  project_id = var.project_id
  tags       = module.context.common_tag_list
}
