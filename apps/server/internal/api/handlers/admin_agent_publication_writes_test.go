package handlers

import "testing"

func TestPublicationDraftWriteIntent(t *testing.T) {
	cases := []struct {
		prompt, operation string
		allowed           bool
	}{
		{"Create a draft post for our launch", "publication-create", true},
		{"Please update the draft post", "publication-update", true},
		{"Create a draft post", "publication-update", false},
		{"List posts", "publication-create", false},
		{"How do I create a draft post?", "publication-create", false},
		{"Do not create a draft post", "publication-create", false},
		{"Schedule the draft post for next week", "publication-schedule", true},
		{"Reschedule the post to Friday", "publication-reschedule", true},
		{"Schedule the post", "publication-reschedule", false},
		{"Reschedule the post", "publication-schedule", false},
		{"How do I schedule a post?", "publication-schedule", false},
		{"Don't reschedule the publication", "publication-reschedule", false},
		{"Publish the post now", "publication-publish-now", true},
		{"Send this publication immediately", "publication-publish-now", true},
		{"Post the draft now", "publication-publish-now", true},
		{"Now publish the post to LinkedIn", "publication-publish-now", true},
		{"Please publish our launch post right away", "publication-publish-now", true},
		{"Send the posts ASAP", "publication-publish-now", true},
		{"Publish now", "publication-publish-now", false},
		{"Send immediately", "publication-publish-now", false},
		{"Draft a post for publication tomorrow", "publication-publish-now", false},
		{"Prepare a post for review", "publication-publish-now", false},
		{"Schedule the post for tomorrow", "publication-publish-now", false},
		{"Schedule the post to publish now", "publication-publish-now", false},
		{"Preview the post, then publish the post now", "publication-publish-now", false},
		{"Please explain how to publish the post now", "publication-publish-now", false},
		{"How do I publish a post now?", "publication-publish-now", false},
		{"Please publish the post now?", "publication-publish-now", false},
		{"Do not publish the post now", "publication-publish-now", false},
		{"Never publish this post now", "publication-publish-now", false},
		{"Avoid sending the publication immediately", "publication-publish-now", false},
		{"Publish the post now if it is approved", "publication-publish-now", false},
		{"Publish the post tomorrow and check now", "publication-publish-now", false},
		{"Publish the workflow now", "publication-publish-now", false},
	}
	for _, tc := range cases {
		if got := explicitPublicationIntent(tc.prompt, tc.operation); got != tc.allowed {
			t.Errorf("intent %q %q: got %v", tc.prompt, tc.operation, got)
		}
	}
}
