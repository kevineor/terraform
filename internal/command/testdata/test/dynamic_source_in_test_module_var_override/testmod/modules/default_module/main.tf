resource "test_resource" "foo" {
  value = "from_default"
}

output "value" {
  value = test_resource.foo.value
}
