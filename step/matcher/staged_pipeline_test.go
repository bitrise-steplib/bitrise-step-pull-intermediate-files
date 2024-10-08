package matcher

import (
	"sort"
	"testing"

	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
	"github.com/stretchr/testify/assert"
)

func TestStagedMatcher(t *testing.T) {
	finishedStages := model.FinishedStages{
		{
			Name: "stage1",
			Workflows: []model.Workflow{
				{
					Name:       "workflow1_1",
					ExternalId: "build1_1",
				},
				{
					Name:       "workflow1_2", // we have two of these workflow names in this stage
					ExternalId: "build1_2a",
				},
				{
					Name:       "workflow1_2",
					ExternalId: "build1_2b",
				},
				{
					Name:       "workflow1_2",
					ExternalId: "", // workflows without external IDs should be skipped
				},
			},
		},
		{
			Name: "stage2",
			Workflows: []model.Workflow{
				{
					Name:       "workflow2",
					ExternalId: "build2",
				},
			},
		},
		{
			Name: "stage3",
			Workflows: []model.Workflow{
				{
					Name:       "workflow1_1",
					ExternalId: "build3",
				},
			},
		},
		{
			Name: "stage4",
			Workflows: []model.Workflow{
				{
					Name:       "workflow3",
					ExternalId: "build4",
				},
			},
		},
	}
	testCases := []struct {
		desc                 string
		finishedStages       model.FinishedStages
		targetNames          []string
		expectedBuildIDs     []string
		expectedErrorMessage string
	}{
		{
			desc:                 "when user defines stage names, it return the build IDs",
			targetNames:          []string{"stage1.*", "stage2.*"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build1_1", "build1_2a", "build1_2b", "build2"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when user defines targets and the result has common subset, it filters the duplications",
			targetNames:          []string{"stage1.*", ".*workflow1_1"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build1_1", "build1_2a", "build1_2b", "build3"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when user defines workflow names, it return the build IDs",
			targetNames:          []string{".*workflow1_1", ".*workflow2"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build1_1", "build3", "build2"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when user wants to query all generated artifacts",
			targetNames:          []string{".*"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build1_1", "build1_2a", "build1_2b", "build2", "build3", "build4"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when user wants to get an exact workflow of the stages build",
			targetNames:          []string{"stage4\\.workflow3"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build4"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when user does not define target names, it returns with all build ids",
			targetNames:          []string{},
			finishedStages:       finishedStages,
			expectedBuildIDs:     []string{"build1_1", "build1_2a", "build1_2b", "build2", "build3", "build4"},
			expectedErrorMessage: "",
		},
		{
			desc:                 "when given stage name not found",
			targetNames:          []string{"wrong_stage_name"},
			finishedStages:       finishedStages,
			expectedBuildIDs:     nil,
			expectedErrorMessage: "",
		},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			matcher := newStagedPipelineMatcher(tC.finishedStages, tC.targetNames, log.NewLogger())

			buildIDs, err := matcher.Matches()
			if tC.expectedErrorMessage != "" {
				assert.EqualError(t, err, tC.expectedErrorMessage)
			} else {
				assert.NoError(t, err)
			}

			sort.Strings(buildIDs)
			sort.Strings(tC.expectedBuildIDs)

			assert.Equal(t, tC.expectedBuildIDs, buildIDs)
		})
	}
}
