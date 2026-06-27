variable "required_input" {
  type = string
  # no default - caller must supply this variable
}

output "value" {
  value = var.required_input
}
