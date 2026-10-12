package ui

import "github.com/charmbracelet/lipgloss"

var (
	colAccent = lipgloss.AdaptiveColor{Light: "#5A4FCF", Dark: "#9D8CFF"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#8A8A8A"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#A15C00", Dark: "#F2B84B"}
	colErr    = lipgloss.AdaptiveColor{Light: "#C0262D", Dark: "#FF6B6B"}
	colOK     = lipgloss.AdaptiveColor{Light: "#1E7A3A", Dark: "#5FD38D"}
	colSelBg  = lipgloss.AdaptiveColor{Light: "#E4E0FF", Dark: "#2F2A55"}

	stTitle     = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stZone      = lipgloss.NewStyle().Bold(true)
	stSource    = lipgloss.NewStyle().Foreground(colAccent)
	stMuted     = lipgloss.NewStyle().Foreground(colMuted)
	stHeader    = lipgloss.NewStyle().Foreground(colMuted).Bold(true)
	stSelected  = lipgloss.NewStyle().Background(colSelBg).Bold(true)
	stPending   = lipgloss.NewStyle().Foreground(colWarn)
	stDeleted   = lipgloss.NewStyle().Foreground(colErr).Strikethrough(true)
	stErr       = lipgloss.NewStyle().Foreground(colErr)
	stOK        = lipgloss.NewStyle().Foreground(colOK)
	stKey       = lipgloss.NewStyle().Bold(true)
	stHighlight = lipgloss.NewStyle().Reverse(true)
	stChoice    = lipgloss.NewStyle().Bold(true).Background(colAccent).Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#1E1E2E"})
	stLabel     = lipgloss.NewStyle().Width(7).Foreground(colMuted)
	stLabelOn   = lipgloss.NewStyle().Width(7).Foreground(colAccent).Bold(true)
)
