package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopic creates the topic when it is missing. An existing topic is left as it is.
func EnsureTopic(ctx context.Context, brokers []string, topic string, partitions int) error {
	if partitions < 1 {
		partitions = 1
	}
	client := &kafka.Client{
		Addr:      kafka.TCP(brokers...),
		Transport: &kafka.Transport{DialTimeout: 2 * time.Second},
	}
	res, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{
			Topic:             topic,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		}},
	})
	if err != nil {
		return fmt.Errorf("create topic %s: %w", topic, err)
	}
	for name, topicErr := range res.Errors {
		if topicErr == nil || isTopicExists(topicErr) {
			continue
		}
		return fmt.Errorf("create topic %s: %w", name, topicErr)
	}
	return nil
}

func isTopicExists(err error) bool {
	var kerr kafka.Error
	if errors.As(err, &kerr) && kerr == kafka.TopicAlreadyExists {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "topic_already_exists")
}
