// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// literalExpr returns a static hcl.Expression wrapping the given string value,
// suitable for use in runVariableBody tests.
func literalExpr(s string) hcl.Expression {
	return hcl.StaticExpr(cty.StringVal(s), hcl.Range{})
}

func TestRunVariableBody_Content_providedAttribute(t *testing.T) {
	body := runVariableBody{
		exprs: map[string]hcl.Expression{"foo": literalExpr("bar")},
		rng:   hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "foo", Required: true}},
	}

	content, diags := body.Content(schema)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if _, ok := content.Attributes["foo"]; !ok {
		t.Errorf("expected attribute 'foo' in content")
	}
}

func TestRunVariableBody_Content_missingRequired(t *testing.T) {
	body := runVariableBody{
		exprs: map[string]hcl.Expression{},
		rng:   hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "required_var", Required: true}},
	}

	_, diags := body.Content(schema)
	if !diags.HasErrors() {
		t.Errorf("expected error for missing required attribute, got none")
	}
}

func TestRunVariableBody_Content_missingOptional(t *testing.T) {
	body := runVariableBody{
		exprs: map[string]hcl.Expression{},
		rng:   hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "optional_var", Required: false}},
	}

	content, diags := body.Content(schema)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics for missing optional attribute: %s", diags)
	}
	if _, ok := content.Attributes["optional_var"]; ok {
		t.Errorf("expected missing optional attribute to be absent from content")
	}
}

func TestRunVariableBody_Content_unexpectedAttribute(t *testing.T) {
	body := runVariableBody{
		exprs: map[string]hcl.Expression{
			"known":   literalExpr("v"),
			"unknown": literalExpr("x"),
		},
		rng: hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "known", Required: false}},
	}

	_, diags := body.Content(schema)
	if !diags.HasErrors() {
		t.Errorf("expected error for unexpected attribute 'unknown', got none")
	}
}

func TestRunVariableBody_PartialContent_leavesUnknownInRemain(t *testing.T) {
	body := runVariableBody{
		exprs: map[string]hcl.Expression{
			"consumed": literalExpr("a"),
			"extra":    literalExpr("b"),
		},
		rng: hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "consumed", Required: false}},
	}

	content, remain, diags := body.PartialContent(schema)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if _, ok := content.Attributes["consumed"]; !ok {
		t.Errorf("expected 'consumed' in content")
	}

	remainBody, ok := remain.(runVariableBody)
	if !ok {
		t.Fatalf("remain is not a runVariableBody")
	}
	if _, ok := remainBody.exprs["extra"]; !ok {
		t.Errorf("expected 'extra' to remain in the leftover body")
	}
	if _, ok := remainBody.exprs["consumed"]; ok {
		t.Errorf("expected 'consumed' to be removed from the leftover body")
	}
}

func TestRunVariableBody_JustAttributes(t *testing.T) {
	expr := literalExpr("hello")
	body := runVariableBody{
		exprs: map[string]hcl.Expression{"greeting": expr},
		rng:   hcl.Range{},
	}

	attrs, diags := body.JustAttributes()
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	attr, ok := attrs["greeting"]
	if !ok {
		t.Fatalf("expected attribute 'greeting'")
	}
	if attr.Expr != expr {
		t.Errorf("expected expression to be the original, got different instance")
	}
}

func TestRunVariableBody_expressionPreserved(t *testing.T) {
	// Verify that the expression returned via Content is the original
	// hcl.Expression, not a re-wrapped literal, so callers get the real AST.
	src := `"hello"`
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}

	body := runVariableBody{
		exprs: map[string]hcl.Expression{"v": expr},
		rng:   hcl.Range{},
	}
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "v"}},
	}

	content, diags := body.Content(schema)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if content.Attributes["v"].Expr != expr {
		t.Errorf("Content did not preserve the original expression")
	}
}
