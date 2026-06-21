run "test" {
  module {
    source = "./testmod"
  }

  assert {
    condition     = module.child.value == "from_child"
    error_message = "expected from_child from dynamically sourced module within the test module"
  }
}
