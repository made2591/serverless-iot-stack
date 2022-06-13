package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

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

// type of Job for pipelining of function
type Job struct {
	Event  *IoTEvent
	Result string
	Error  error
}

// ****************************************************
// ******************* VARS & CONS ********************
// ****************************************************

var (
	err           error
	historyBucket string
	tableName     string
	unixNow       string
	ttlDynamo     int64
	s3svc         *s3manager.Uploader
	dynamodbsvc   *dynamodb.DynamoDB
	cwsvc         *cloudwatch.CloudWatch
)

const (
	Monitor Action = iota
	Remediate
	TTL_DYNAMO = 60
)

// ****************************************************
// ********************* HELPERS **********************
// ****************************************************

func init() {

	// set logger
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
	historyBucket = os.Getenv("HISTORY_BUCKET")
	tableName = os.Getenv("MONITORING_TABLE")

	// init ttl dynamo
	ttlDynamo, err = strconv.ParseInt(os.Getenv("TTL_DYNAMO"), 10, 64)
	if err != nil {
		ttlDynamo = TTL_DYNAMO
	}
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION")),
	}))

	// init services
	s3svc = s3manager.NewUploader(sess)
	dynamodbsvc = dynamodb.New(sess)
	cwsvc = cloudwatch.New(sess)

}

// map the integer value of an action to its corresponding value
func (d Action) String() string {
	return [...]string{"Monitor", "Remediate"}[d]
}

// ****************************************************
// ****************** CORE FUNCTION *******************
// ****************************************************

// publish on Cloudwatch metrics for the specific device using the information in the message
func publishMetric(m *Job, r chan *Job) {
	_, err := cwsvc.PutMetricData(&cloudwatch.PutMetricDataInput{
		Namespace: aws.String("Device/Monitoring"),
		MetricData: []*cloudwatch.MetricDatum{
			&cloudwatch.MetricDatum{
				MetricName: aws.String("Temperature"),
				Unit:       aws.String("None"),
				Value:      aws.Float64(m.Event.Body.Temp),
				Dimensions: []*cloudwatch.Dimension{
					&cloudwatch.Dimension{
						Name:  aws.String("Device"),
						Value: aws.String(m.Event.Body.Device),
					},
				},
			},
			&cloudwatch.MetricDatum{
				MetricName: aws.String("Humidity"),
				Unit:       aws.String("None"),
				Value:      aws.Float64(m.Event.Body.Hum),
				Dimensions: []*cloudwatch.Dimension{
					&cloudwatch.Dimension{
						Name:  aws.String("Device"),
						Value: aws.String(m.Event.Body.Device),
					},
				},
			},
		},
	})
	if err != nil {
		log.Error(fmt.Sprintf("Error in publish metric: %s", err))
	}
	r <- &Job{Event: m.Event, Result: m.Event.Body.Action, Error: err}
}

// historicize on s3 metrics for the specific device using the information in the message
func historicizeOnS3Bucket(m *Job, r chan *Job) {
	b, _ := json.Marshal(m.Event)
	log.Debugf("Bucket: %s", historyBucket)
	log.Debugf("EventKey: %s", unixNow)
	s3r, err := s3svc.Upload(&s3manager.UploadInput{
		Bucket: aws.String(historyBucket),
		Key:    aws.String(unixNow),
		Body:   bytes.NewReader(b),
	})
	res := ""
	if err != nil {
		log.Error(fmt.Sprintf("Error in object upload: %s", err))
	} else {
		dmy, _ := json.Marshal(s3r)
		res = string(dmy)
	}
	r <- &Job{Event: m.Event, Result: res, Error: err}
}

// persist on DynamoDB metrics for the specific device using the information in the message
func persistOnDynamoDB(m *Job, r chan *Job) {
	ttl, _ := strconv.ParseInt(unixNow, 10, 64)
	i := &Item{
		Digest: unixNow,
		Device: m.Event.Body.Device,
		Temp:   m.Event.Body.Temp,
		Hum:    m.Event.Body.Hum,
		Action: m.Event.Body.Action,
		TTL:    ttl + ttlDynamo,
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
	dar, err := dynamodbsvc.PutItem(input)
	res := ""
	if err != nil {
		log.Errorf("Error in PutItem: %s", err)
	} else {
		dmy, _ := json.Marshal(dar)
		res = string(dmy)
	}
	r <- &Job{Event: m.Event, Result: res, Error: err}
}

// ****************************************************
// **************** MONADIC REASONING *****************
// ****************************************************

// operator Type function to chain actions
type Operator func(m *Job, r chan *Job)

// encapsulate the event in a Job
func unit(c IoTEvent) *Job {

	return &Job{Event: &c, Result: "", Error: nil}

}

// chain the operation over specific Job in a concurrent way
func pipeline(m *Job, os ...Operator) <-chan *Job {

	r := make(chan *Job, len(os))
	for i, o := range os {
		e, _ := json.Marshal(m.Event)
		log.Infof("Processing %d: %s", i, bytes.NewBuffer(e).String())
		go o(m, r)
	}
	return r

}

// consume result for the specific Job
func consume(r <-chan *Job, wg *sync.WaitGroup) {

	defer wg.Done()
	m := <-r
	if m.Error != nil {
		log.Errorf("Error in consume: %s", m.Error)
	}

}

// lambda handler
func handler(event IoTEvent) {

	// isolate unix timestamp
	unixNow = strconv.FormatInt(time.Now().Unix(), 10)

	// load event
	e, _ := json.Marshal(event)
	log.Infof("Time start %s dispatch event: %+v", unixNow, string(e))

	// init a Jobs pipeline
	var wg sync.WaitGroup
	Jobs := pipeline(
		unit(event),
		publishMetric,
		historicizeOnS3Bucket,
		persistOnDynamoDB,
	)

	// consume the result
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go consume(Jobs, &wg)
	}
	wg.Wait()

	finish := strconv.FormatInt(time.Now().Unix(), 10)
	log.Infof("Time end %s dispatch event: %+v", finish, bytes.NewBuffer(e).String())

}

func main() {
	// if false {
	// 	var iotEvent IoTEvent
	// 	json.Unmarshal([]byte(os.Args[1]), &iotEvent)
	// 	handler(iotEvent)
	// } else {
	// 	lambda.Start(handler)
	// }
	lambda.Start(handler)
}
