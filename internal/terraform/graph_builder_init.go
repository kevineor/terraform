// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type InitGraphBuilder struct {
	// A config derived from the root module
	Config *configs.Config

	RootVariableValues InputValues

	Walker configs.ModuleWalker

	// CallExpressions, if set, provides the call-site argument expression for
	// each of Config's input variables instead of looking them up from a module
	// call block. It is only meaningful when Config is a non-root module and is
	// used for test run modules, whose call site is a run block (see
	// nodeInstallTestRunModule).
	CallExpressions map[string]hcl.Expression
}

// See GraphBuilder
func (b *InitGraphBuilder) Build(path addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics) {
	log.Printf("[TRACE] building graph for terraform dependencies")
	return (&BasicGraphBuilder{
		Steps: b.Steps(),
		Name:  "InitGraphBuilder",
	}).Build(path)
}

// See GraphBuilder
func (b *InitGraphBuilder) Steps() []GraphTransformer {
	steps := []GraphTransformer{}

	if b.Config.Parent == nil {
		steps = append(steps, &RootVariableTransformer{
			Config:         b.Config,
			RawValues:      b.RootVariableValues,
			ValidateChecks: true,
		})
	} else {
		steps = append(steps, &ModuleVariableTransformer{
			Config:          b.Config,
			ModuleOnly:      true,
			ValidateChecks:  true,
			CallExpressions: b.CallExpressions,
		})
	}

	steps = append(steps, []GraphTransformer{
		&ModuleTransformer{
			Config: b.Config,
			Walker: b.Walker,
		},

		// Install the alternative modules referenced by test run blocks so
		// that their descendant modules (including any with dynamic source
		// addresses) are resolved through the init graph as well.
		&TestModuleTransformer{
			Config: b.Config,
			Walker: b.Walker,
		},

		&LocalTransformer{
			Config: b.Config,
		},

		&ReferenceTransformer{},

		// Filters out any vertices that aren't relevant to the init graph
		&TransformFilter{
			Keep: func(v dag.Vertex) bool {
				switch n := v.(type) {
				case *nodeInstallModule:
					return true
				case *nodeInstallTestRunModule:
					return true
				case *NodeRootVariable:
					return n.Config.Const
				case *nodeExpandModuleVariable:
					return n.Config.Const
				default:
					return false
				}
			},
		},

		&variableValidationTransformer{
			operation: walkInit,
		},

		&RootTransformer{},

		&TransitiveReductionTransformer{},
	}...)

	return steps
}
