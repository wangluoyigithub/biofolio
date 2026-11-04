package main

import "testing"

func TestCompleteIntake(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"questions":[{"answer":"C","a":"依据题干判断"}]}`, true},
		{`{"questions":[{"answerHtml":"<p>A</p>","analysisHtml":"<p> &nbsp; </p>"}]}`, false},
		{`{"questions":[{"answer":"待补答案","a":"解析"}]}`, false},
		{`{"questions":[{"answer":"A","a":"解释"},{}]}`, false},
		{`{"links":[{"q":{"a":"解释"}}]}`, false},
		{`{"questions":[],"paper":true}`, true},
	} {
		if got := completeIntake([]byte(tc.body)); got != tc.ok {
			t.Errorf("completeIntake(%s)=%v", tc.body, got)
		}
	}
}
