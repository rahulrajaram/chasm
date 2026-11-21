package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const repositoryFreshnessWidth = 13

type humanRepository struct {
	Name       string
	Sandbox    string
	Path       string
	Freshness  string
	ObservedAt string
}

type humanSandboxObservation struct {
	Name       string
	Freshness  string
	ObservedAt string
}

type humanRepositoryInventory struct {
	Repositories []humanRepository
	Sandboxes    []humanSandboxObservation
	Complete     bool
}

func parseHumanRepositoryInventory(result map[string]interface{}, repositoryField string) (humanRepositoryInventory, error) {
	complete, completeOK := result["complete"].(bool)
	repositories, repositoriesOK := result[repositoryField].([]interface{})
	sandboxes, sandboxesOK := result["sandboxes"].([]interface{})
	if !completeOK || !repositoriesOK || !sandboxesOK {
		return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is malformed")
	}
	parsedRepositories := make([]humanRepository, 0, len(repositories))
	for _, candidate := range repositories {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is malformed")
		}
		repository := humanRepository{
			Name: stringField(item, "name"), Sandbox: stringField(item, "logical_name"),
			Path: stringField(item, "path"), Freshness: stringField(item, "freshness"), ObservedAt: stringField(item, "observed_at"),
		}
		if repository.Name == "" || repository.Sandbox == "" || !isWorkspacePath(repository.Path) ||
			!validObservation(repository.Freshness, repository.ObservedAt, false) {
			return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is malformed")
		}
		parsedRepositories = append(parsedRepositories, repository)
	}
	parsedSandboxes := make([]humanSandboxObservation, 0, len(sandboxes))
	for _, candidate := range sandboxes {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is malformed")
		}
		sandbox := humanSandboxObservation{
			Name: stringField(item, "logical_name"), Freshness: stringField(item, "freshness"), ObservedAt: stringField(item, "observed_at"),
		}
		if sandbox.Name == "" || !validObservation(sandbox.Freshness, sandbox.ObservedAt, true) {
			return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is malformed")
		}
		parsedSandboxes = append(parsedSandboxes, sandbox)
	}
	if complete == hasCompleteObservations(parsedSandboxes) {
		return humanRepositoryInventory{Repositories: parsedRepositories, Sandboxes: parsedSandboxes, Complete: complete}, nil
	}
	return humanRepositoryInventory{}, fmt.Errorf("repository inventory response is inconsistent")
}

func isWorkspacePath(path string) bool {
	return path == "/workspace" || strings.HasPrefix(path, "/workspace/")
}

func stringField(item map[string]interface{}, name string) string {
	value, _ := item[name].(string)
	return value
}

func validObservation(freshness, observedAt string, allowUnobserved bool) bool {
	switch freshness {
	case "live", "last_observed":
		_, err := time.Parse(time.RFC3339, observedAt)
		return err == nil
	case "unobserved":
		return allowUnobserved && observedAt == ""
	default:
		return false
	}
}

func hasCompleteObservations(sandboxes []humanSandboxObservation) bool {
	for _, sandbox := range sandboxes {
		if sandbox.Freshness == "unobserved" {
			return false
		}
	}
	return true
}

func renderRepositoryInventory(output io.Writer, inventory humanRepositoryInventory, query string, style humanOutputStyle) {
	if len(inventory.Repositories) == 0 {
		if query == "" {
			fmt.Fprintln(output, "No repositories observed.")
		} else {
			fmt.Fprintf(output, "No observed repository named %q.\n", query)
		}
	} else {
		renderRepositoryTable(output, inventory.Repositories, style)
	}
	if !inventory.Complete {
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Inventory incomplete")
		for _, sandbox := range inventory.Sandboxes {
			if sandbox.Freshness == "unobserved" {
				fmt.Fprintf(output, "  %s — repository contents unknown\n", escapeHumanText(sandbox.Name))
			}
		}
	}
	fmt.Fprintln(output)
	unit := "repositories"
	if len(inventory.Repositories) == 1 {
		unit = "repository"
	}
	state := "complete"
	if !inventory.Complete {
		state = "incomplete"
	}
	fmt.Fprintf(output, "%d %s · %d %s · inventory %s\n", len(inventory.Repositories), unit,
		len(inventory.Sandboxes), pluralize("sandbox", len(inventory.Sandboxes)), state)
}

func renderRepositoryTable(output io.Writer, repositories []humanRepository, style humanOutputStyle) {
	nameWidth := columnWidth("REPOSITORY", repositories, func(repository humanRepository) string { return repository.Name }, 32)
	sandboxWidth := columnWidth("SANDBOX", repositories, func(repository humanRepository) string { return repository.Sandbox }, 32)
	pathWidth := style.Width - nameWidth - sandboxWidth - repositoryFreshnessWidth - 6
	if style.Width >= minimumWideTableWidth && pathWidth >= 16 {
		fmt.Fprintf(output, "%-*s  %-*s  %-*s  %-*s\n", nameWidth, "REPOSITORY", sandboxWidth, "SANDBOX",
			repositoryFreshnessWidth, "FRESHNESS", pathWidth, "PATH")
		for _, repository := range repositories {
			fmt.Fprintf(output, "%-*s  %-*s  %-*s  %-*s\n", nameWidth, truncateCell(repository.Name, nameWidth),
				sandboxWidth, truncateCell(repository.Sandbox, sandboxWidth), repositoryFreshnessWidth,
				freshnessLabel(repository.Freshness), pathWidth, truncateCell(repository.Path, pathWidth))
		}
		return
	}
	fmt.Fprintf(output, "%-*s  %-*s  %-*s\n", nameWidth, "REPOSITORY", sandboxWidth, "SANDBOX", repositoryFreshnessWidth, "FRESHNESS")
	for _, repository := range repositories {
		fmt.Fprintf(output, "%-*s  %-*s  %-*s\n", nameWidth, truncateCell(repository.Name, nameWidth), sandboxWidth,
			truncateCell(repository.Sandbox, sandboxWidth), repositoryFreshnessWidth, freshnessLabel(repository.Freshness))
		fmt.Fprintf(output, "  %s\n", escapeHumanText(repository.Path))
	}
}

func columnWidth(header string, repositories []humanRepository, value func(humanRepository) string, maximum int) int {
	width := cellWidth(header)
	for _, repository := range repositories {
		if candidate := cellWidth(value(repository)); candidate > width {
			width = candidate
		}
	}
	if width > maximum {
		return maximum
	}
	return width
}

func freshnessLabel(freshness string) string {
	if freshness == "last_observed" {
		return "last observed"
	}
	return freshness
}
