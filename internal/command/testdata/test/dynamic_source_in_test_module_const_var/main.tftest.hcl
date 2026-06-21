run "test" {
  module {
    source = "./testmod"
  }

  assert {
    condition     = module.mod.value == "bar"
    error_message = "expected bar from a const-driven dynamic source within the test module"
  }
}
