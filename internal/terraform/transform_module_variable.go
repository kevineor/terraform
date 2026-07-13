// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/tfdiags"

	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/configs"
)

// ModuleVariableTransformer is a GraphTransformer that adds all the variables
// in the configuration to the graph.
//
// Any "variable" block present in any non-root module is included here, even
// if a particular variable is not referenced from anywhere.
//
// The transform will produce errors if a call to a module does not conform
// to the expected set of arguments, but this transformer is not in a good
// position to return errors and so the validate walk should include specific
// steps for validating module blocks, separate from this transform.
type ModuleVariableTransformer struct {
	Config *configs.Config

	// ModuleOnly, if true, makes the transformer only process the
	// variables in the current module, skipping any child modules.
	ModuleOnly bool

	// CallExpressions, if set, provides the call-site argument expression for
	// each of Config's input variables (keyed by variable name) instead of
	// looking them up from a module call block in the parent module. It is only
	// honored in ModuleOnly mode and is used for test run modules, whose call
	// site is a run block rather than a module call. Unlike a module call
	// block, these expressions are not schema-validated: variables absent from
	// the map fall back to their default, and extra entries are ignored.
	CallExpressions map[string]hcl.Expression

	// ValidateChecks should be set to true if the graph should run the user-defined validations for child module variables
	ValidateChecks bool

	// DestroyApply must be set to true when applying a destroy operation and
	// false otherwise.
	DestroyApply bool
}

func (t *ModuleVariableTransformer) Transform(g *Graph) error {
	if t.ModuleOnly && t.Config.Parent != nil {
		return t.transformSingle(g, t.Config.Parent, t.Config)
	} else {
		return t.transform(g, nil, t.Config)
	}
}

func (t *ModuleVariableTransformer) transform(g *Graph, parent, c *configs.Config) error {
	// We can have no variables if we have no configuration.
	if c == nil {
		return nil
	}

	// Transform all the children first.
	for _, cc := range c.Children {
		if err := t.transform(g, c, cc); err != nil {
			return err
		}
	}

	// If we're processing anything other than the root module then we'll
	// add graph nodes for variables defined inside. (Variables for the root
	// module are dealt with in RootVariableTransformer).
	// If we have a parent, we can determine if a module variable is being
	// used, so we transform this.
	if parent != nil {
		if err := t.transformSingle(g, parent, c); err != nil {
			return err
		}
	}

	return nil
}

func (t *ModuleVariableTransformer) transformSingle(g *Graph, parent, c *configs.Config) error {
	// Determine the call-site expression for each of c's input variables.
	// Normally these come from the module call block in the parent module, but
	// a caller may supply them directly via CallExpressions (e.g. for test run
	// modules, whose call site is a run block rather than a module call).
	exprs := t.CallExpressions
	if exprs == nil {
		_, call := c.Path.Call()
		callConfig, exists := parent.Module.ModuleCalls[call.Name]
		if !exists {
			// This should never happen, since it indicates an improperly-constructed
			// configuration tree.
			panic(fmt.Errorf("no module call block found for %s", c.Path))
		}

		// We need to construct a schema for the expected call arguments based
		// on the configured variables in our config, which we can then use to
		// decode the content of the call block.
		schema := &hcl.BodySchema{}
		for _, v := range c.Module.Variables {
			schema.Attributes = append(schema.Attributes, hcl.AttributeSchema{
				Name:     v.Name,
				Required: v.Default == cty.NilVal,
			})
		}

		content, contentDiags := callConfig.Config.Content(schema)
		if contentDiags.HasErrors() {
			// Validation code elsewhere should deal with any errors before we
			// get in here, but we'll report them out here just in case, to
			// avoid crashes.
			var diags tfdiags.Diagnostics
			diags = diags.Append(contentDiags)
			return diags.Err()
		}

		exprs = make(map[string]hcl.Expression, len(content.Attributes))
		for name, attr := range content.Attributes {
			exprs[name] = attr.Expr
		}
	}

	for _, v := range c.Module.Variables {
		// A variable absent from exprs gets a nil expression, so it falls back
		// to its declared default during evaluation.
		expr := exprs[v.Name]

		// Add a plannable node, as the variable may expand
		// during module expansion
		node := &nodeExpandModuleVariable{
			Addr: addrs.InputVariable{
				Name: v.Name,
			},
			Module:         c.Path,
			Config:         v,
			Expr:           expr,
			ValidateChecks: t.ValidateChecks,
			DestroyApply:   t.DestroyApply,
		}
		g.Add(node)
	}

	return nil
}
