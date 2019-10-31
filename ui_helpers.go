package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func (u *uiContext) prompt(prompt string) (string, error) {
	line, err := u.in.Line(prompt)
	if err != nil {
		return "", err
	}

	return line, nil
}

func (u *uiContext) promptMultiline(prompt string) (string, error) {
	infoColor.Println(`Enter text, 2 empty lines or "." or ctrl-d to stop:`)

	var lines []string
	oneBlank := false
	for {
		line, err := u.prompt(prompt)
		if err == ErrEnd || line == "." {
			break
		} else if err != nil {
			return "", err
		}

		if line == "." {
			break
		} else if len(line) == 0 {
			if oneBlank {
				break
			}
			oneBlank = true
			continue
		}

		if oneBlank {
			lines = append(lines, "")
			oneBlank = false
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}

// findOne returns a uuid iff a single one could be found, else an error
// message will have been printed to the user.
func (u *uiContext) findOne(query string) (string, error) {
	entries, err := u.store.Search(query)
	if err != nil {
		return "", err
	}

	switch len(entries) {
	case 0:
		errColor.Printf("No matches for query (%q)\n", query)
		return "", nil
	case 1:
		ids := entries.UUIDs()
		id := ids[0]
		name := entries[id]
		if query != name {
			infoColor.Printf("using: %s\n", name)
		}

		return id, nil
	}

	// If there's an exact match use that
	for u, name := range entries {
		if name == query {
			return u, nil
		}
	}

	names := entries.Names()
	sort.Strings(names)
	errColor.Printf("Multiple matches for query (%q):", query)
	fmt.Print("\n  ")
	fmt.Println(strings.Join(names, "\n  "))

	return "", nil
}

// getString ensures a non-empty string
func (u *uiContext) getString(key string) (string, error) {
	var str string
	var err error

Again:
	str, err = u.prompt(promptColor.Sprint(key + ": "))
	if err != nil {
		return "", err
	}
	if len(str) == 0 {
		errColor.Println(key, "cannot be empty")
		goto Again
	}

	return str, nil
}

func (u *uiContext) getInt(key string, min, max int) (int, error) {
	var str string
	var err error
	var integer int

Again:
	str, err = u.prompt(promptColor.Sprint(key + ": "))
	if err != nil {
		return 0, err
	}

	if len(str) == 0 {
		errColor.Println(key, "cannot be empty")
		goto Again
	}

	integer, err = strconv.Atoi(str)
	if err != nil {
		errColor.Printf("%s must be an integer between %d and %d\n", key, min, max)
		goto Again
	}

	return integer, nil
}

func (u *uiContext) getMenuChoice(prompt string, items []string) (int, error) {
	var choice string
	var integer int
	var i int
	var item string
	var err error

Again:
	for i, item = range items {
		promptColor.Printf(" %d) %s\n", i+1, item)
	}
	choice, err = u.prompt(infoColor.Sprint(prompt))
	if err != nil {
		return 0, err
	}

	integer, err = strconv.Atoi(choice)
	if err != nil {
		errColor.Println("invalid choice")
		goto Again
	}

	integer--
	if integer < 0 || integer >= len(items) {
		goto Again
	}

	return integer, nil
}