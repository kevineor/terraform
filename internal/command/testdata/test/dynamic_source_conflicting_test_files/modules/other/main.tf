resource "test_resource" "foo" {
  value = "baz"
}

output "value" {
  value = test_resource.foo.value
}
