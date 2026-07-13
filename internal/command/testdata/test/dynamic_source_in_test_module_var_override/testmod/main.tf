variable "module_name" {
  type    = string
  const   = true
  default = "default_module"
}

module "mod" {
  source = "./modules/${var.module_name}"
}

output "value" {
  value = module.mod.value
}
