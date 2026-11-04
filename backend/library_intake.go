package main

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

var photoStem = regexp.MustCompile(`(?i)^[^\n]+\.(jpe?g|png|webp|gif|heic)$`)
var intakeTags = regexp.MustCompile("(?s)<[^>]+>")
var intakeImage = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*["']data:image/[^;]+;base64,[a-zA-Z0-9+/]`)

func solutionPresent(value string) bool {
	if intakeImage.MatchString(value) {
		return true
	}
	text := strings.TrimSpace(html.UnescapeString(intakeTags.ReplaceAllString(value, "")))
	return text != "" && text != "待补答案" && text != "待补解析"
}
func completeIntake(body []byte) bool {
	type question struct{ Stem, ContentHTML, Answer, AnswerHTML, A, AnalysisHTML string }
	var record struct {
		Question  *question
		Questions []question
		Links     []struct{ Q question }
	}
	if json.Unmarshal(body, &record) != nil {
		return false
	}
	if record.Question != nil {
		record.Questions = append(record.Questions, *record.Question)
	}
	for _, link := range record.Links {
		record.Questions = append(record.Questions, link.Q)
	}
	for _, q := range record.Questions {
		if photoStem.MatchString(strings.TrimSpace(q.Stem)) && strings.Contains(q.ContentHTML, "<img") {
			return false
		}
		if !(solutionPresent(q.Answer) || solutionPresent(q.AnswerHTML)) || !(solutionPresent(q.A) || solutionPresent(q.AnalysisHTML)) {
			return false
		}
	}
	return true
}
