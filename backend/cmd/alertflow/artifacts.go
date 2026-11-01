package main

import (
	"net/http"

	"github.com/ecakeman/alertflow/backend/internal/api"
	agentv1 "github.com/ecakeman/alertflow/gen/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type artifactClient struct {
	conn   *grpc.ClientConn
	client agentv1.AgentExecutionServiceClient
}

func agentArtifacts(target string) api.ArtifactSource {
	if target == "" {
		return nil
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil
	}
	return &artifactClient{conn: conn, client: agentv1.NewAgentExecutionServiceClient(conn)}
}

func (c *artifactClient) GetArtifact(r *http.Request, episodeID uuid.UUID, artifactType string) (any, error) {
	kind := agentv1.ArtifactType_ARTIFACT_TYPE_INVESTIGATION
	if artifactType == "DIAGNOSIS_REPORT" {
		kind = agentv1.ArtifactType_ARTIFACT_TYPE_DIAGNOSIS_REPORT
	}
	resp, err := c.client.GetArtifact(r.Context(), &agentv1.GetArtifactRequest{EpisodeId: episodeID.String(), ArtifactType: kind})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"artifact_type": resp.GetArtifactType().String(),
		"version":       resp.GetVersion(),
		"content_type":  resp.GetContentType(),
		"content_text":  resp.GetContentText(),
		"content_json":  resp.GetContentJson().AsMap(),
		"content_hash":  resp.GetContentHash(),
	}, nil
}
