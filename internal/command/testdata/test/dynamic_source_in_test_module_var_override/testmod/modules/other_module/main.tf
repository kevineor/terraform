resource "test_resource" "foo" {
  value = "from_other"
}

output "value" {
  value = test_resource.foo.value
}
