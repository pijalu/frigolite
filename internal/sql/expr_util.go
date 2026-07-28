// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//

package sql

// UnwrapParenExpr recursively unwraps *ParenExpr nodes to return the
// underlying expression. This allows ParenExpr to be a transparent
// wrapper that does not require explicit case handling in every
// type switch — callers can preprocess with UnwrapParenExpr at
// entry points.
//
// This function is idempotent: calling it on an expression with no
// ParenExpr nodes returns the expression unchanged.
func UnwrapParenExpr(expr Expr) Expr {
	for {
		if p, ok := expr.(*ParenExpr); ok {
			expr = p.Expr
		} else {
			return expr
		}
	}
}
