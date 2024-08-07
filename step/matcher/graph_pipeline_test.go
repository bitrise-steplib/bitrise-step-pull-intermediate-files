package matcher

import (
	"sort"
	"testing"

	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
	"github.com/stretchr/testify/assert"
)

func TestGraphMatcher(t *testing.T) {
	finishedWorkflows := model.FinishedWorkflows{
		{
			Name:       "workflow1_1",
			ExternalId: "build1_1",
		},
		{
			Name:       "workflow1_2",
			ExternalId: "build1_2",
		},
		{
			Name:       "workflow2_1",
			ExternalId: "build2_1",
		},
		{
			Name:       "workflow3_1",
			ExternalId: "build3_1",
		},
		{
			Name:       "workflow3_2",
			ExternalId: "",
		},
		{
			Name:       "workflow4_1",
			ExternalId: "build4_1",
		},
	}
	testCases := []struct {
		desc                 string
		targetNames          []string
		expectedBuildIDs     []string
		expectedErrorMessage string
	}{
		{
			desc:                 "download everything",
			targetNames:          []string{".*"},
			expectedBuildIDs:     []string{"build1_1", "build1_2", "build2_1", "build3_1", "build4_1"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "exact workflow names",
			targetNames:          []string{"workflow1_1", "workflow2_1"},
			expectedBuildIDs:     []string{"build1_1", "build2_1"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "partial workflow name",
			targetNames:          []string{"workflow1.*"},
			expectedBuildIDs:     []string{"build1_1", "build1_2"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "multiple patterns with deduplicated results",
			targetNames:          []string{"workflow1.*", "workflow1_1", "workflow.*"},
			expectedBuildIDs:     []string{"build1_1", "build1_2", "build2_1", "build3_1", "build4_1"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "missing target names",
			targetNames:          []string{},
			expectedBuildIDs:     []string{"build1_1", "build1_2", "build2_1", "build3_1", "build4_1"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "not existing workflow name",
			targetNames:          []string{"missing_workflow_name"},
			expectedBuildIDs:     nil,
			expectedErrorMessage: "",
		},
		{
			desc:                 "invalid regex",
			targetNames:          []string{"["},
			expectedBuildIDs:     nil,
			expectedErrorMessage: "error parsing regexp: missing closing ]: `[`",
		},
	}
	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			matcher := newGraphPipelineMatcher(finishedWorkflows, tt.targetNames, log.NewLogger())

			buildIDs, err := matcher.Matches()
			if tt.expectedErrorMessage != "" {
				assert.EqualError(t, err, tt.expectedErrorMessage)
			} else {
				assert.NoError(t, err)
			}

			sort.Strings(buildIDs)
			sort.Strings(tt.expectedBuildIDs)

			assert.Equal(t, tt.expectedBuildIDs, buildIDs)
		})
	}
}
