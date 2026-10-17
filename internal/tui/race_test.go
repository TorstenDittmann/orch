//go:build race

package tui

// The race detector slows rendering several times over, so wall-clock budgets
// scale with it.
const renderBudgetScale = 5
