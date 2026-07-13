locals {
  child_source = "./child"
}

module "child" {
  source = local.child_source
}

output "value" {
  value = module.child.value
}
