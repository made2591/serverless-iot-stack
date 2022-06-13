package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/iotdataplane"

	log "github.com/sirupsen/logrus"
)

// ****************************************************
// ******************** STRUCT ************************
// ****************************************************

// type of action
type Action int

// type of IoTEvent
type IoTEvent struct {
	Body *Information `json:"body"`
}

// type of Information
type Information struct {
	Device string  `json:"device"`
	Temp   float64 `json:"temperature"`
	Hum    float64 `json:"humidity"`
	Action string  `json:"action"`
}

// type of Item
type Item struct {
	Digest string  `json:"digest"`
	Device string  `json:"device"`
	Temp   float64 `json:"temperature"`
	Hum    float64 `json:"humidity"`
	Action string  `json:"action"`
	TTL    int64   `json:"ttl"`
}

// ****************************************************
// ******************* VARS & CONS ********************
// ****************************************************

var (
	remediationTopic string
	tableName        string
	unixNow          string
	logger           *log.Logger
	dynamodbsvc      *dynamodb.DynamoDB
	iotsvc           *iotdataplane.IoTDataPlane
)

const (
	Monitor Action = iota
	Remediate
)

// ****************************************************
// ********************* HELPERS **********************
// ****************************************************

// map the integer value of an action to its corresponding value
func (d Action) String() string {
	return [...]string{"Monitor", "Remediate"}[d]
}

func init() {
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)
	logLevelStr := os.Getenv("LOG_LEVEL")
	if strings.Compare(logLevelStr, "ERROR") == 0 {
		log.SetLevel(log.ErrorLevel)
	}
	if strings.Compare(logLevelStr, "WARNING") == 0 {
		log.SetLevel(log.WarnLevel)
	}
	if strings.Compare(logLevelStr, "DEBUG") == 0 {
		log.SetLevel(log.DebugLevel)
	}
	remediationTopic = os.Getenv("REMEDIATION_TOPIC")
	tableName = os.Getenv("REMEDIATION_TABLE")
	iotsvc = iotdataplane.New(session.Must(session.NewSession(&aws.Config{
		Region:   aws.String(os.Getenv("REGION")),
		Endpoint: aws.String(os.Getenv("IOT_CORE_ENDPOINT")),
	})))
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION")),
	}))
	dynamodbsvc = dynamodb.New(sess)
}

// persist on DynamoDB metrics for the specific device using the information in the message
func persistOnDynamoDB(event *IoTEvent) {
	i := &Item{
		Digest: unixNow,
		Device: event.Body.Device,
		Temp:   event.Body.Temp,
		Hum:    event.Body.Hum,
		Action: event.Body.Action,
	}
	log.Debugf("Dynamo table name: %s", tableName)
	dae, err := dynamodbattribute.MarshalMap(i)
	if err != nil {
		log.Error(fmt.Sprintf("Error in dynamodbattribute: %s", err))
	}
	input := &dynamodb.PutItemInput{
		Item:      dae,
		TableName: aws.String(tableName),
	}
	_, err = dynamodbsvc.PutItem(input)
	if err != nil {
		log.Errorf("Error in PutItem: %s", err)
	}
}

// ****************************************************
// ****************** CORE FUNCTION *******************
// ****************************************************

// remediation logic
func remediationLogic(stream events.DynamoDBEvent) *IoTEvent {
	var newTemperature, oldTemperature float64
	var newHumidity, oldHumidity float64
	var deviceId string
	for _, record := range stream.Records {
		log.Debugf("Processing request data for event ID %s, type %s.\n", record.EventID, record.EventName)
		for name, value := range record.Change.NewImage {
			if strings.Compare(name, "device") == 0 {
				deviceId = value.String()
				log.Debugf("Attribute name: %s, device: %s\n", name, deviceId)
			}
			if strings.Compare(name, "temperature") == 0 {
				newTemperature, _ = value.Float()
				log.Debugf("Attribute name: %s, value: %f\n", name, newTemperature)
			}
			if strings.Compare(name, "humidity") == 0 {
				newHumidity, _ = value.Float()
				log.Debugf("Attribute name: %s, value: %f\n", name, newHumidity)
			}
		}
		for name, value := range record.Change.OldImage {
			if strings.Compare(name, "temperature") == 0 {
				oldTemperature, _ = value.Float()
				log.Debugf("Attribute name: %s, value: %f\n", name, oldTemperature)
			}
			if strings.Compare(name, "humidity") == 0 {
				oldHumidity, _ = value.Float()
				log.Debugf("Attribute name: %s, value: %f\n", name, oldHumidity)
			}
		}
	}
	if newTemperature > oldTemperature {
		log.Debugf("Remediate by cooling down environment: %f, value: %f\n", oldTemperature, oldHumidity)
	} else {
		log.Debugf("Remediate by warming up environment: %f, value: %f\n", oldTemperature, oldHumidity)
	}
	if oldTemperature == 0 || oldHumidity == 0 {
		oldTemperature = newTemperature
		oldHumidity = newHumidity
	}
	remediationMessage := &IoTEvent{Body: &Information{Device: deviceId, Temp: oldTemperature, Hum: oldHumidity, Action: Remediate.String()}}
	persistOnDynamoDB(remediationMessage)
	return remediationMessage
}

// lambda handler
func handler(stream events.DynamoDBEvent) {

	// isolate unix timestamp
	unixNow = strconv.FormatInt(time.Now().Unix(), 10)

	e, _ := json.Marshal(stream)
	if strings.Compare(os.Getenv("REMEDIATION_LOGIC"), "true") == 0 {
		log.Infof("Remediation logic enabled for event: %s", string(e))
		event := remediationLogic(stream)
		payload, _ := json.Marshal(event)
		res, err := iotsvc.Publish(&iotdataplane.PublishInput{
			Topic:   aws.String(remediationTopic),
			Payload: payload,
			Qos:     aws.Int64(0),
		})
		if err != nil {
			log.Errorf("Error in iot publish: %s", err)
		}
		log.Infof("Remediation message sent: %s", string(payload))
		log.Debugf("Result: %s", res)
	} else {
		log.Infof("Remediation logic disabled for event: %s", string(e))
	}
}

func main() {
	lambda.Start(handler)
}
