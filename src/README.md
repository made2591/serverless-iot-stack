### Monitoring

The `monitoring` folder contains the CLI used to simulate the monitoring environment, together with actuators inside as well. Formelly, it pushes messages forever, and listens for some remediation messages to change the generation function the populute produced messages. You can run

```go run main.go --help```

to get all available parameters:

| Parameter          | Environment Var    | Description                                                                    | Default       |
|--------------------|--------------------|--------------------------------------------------------------------------------|---------------|
| device-id          | DEVICE_ID          | The device ID used also for dashboard and metrics                              | your-device   |
| iot-endpoint       | IOT_CORE_ENDPOINT  | Your AWS IoT Core endpoint                                                     | CHANGE_ME |
| velocity           | VELOCITY           | The multiplier factor in the sin(x) function for monitoring message generator  | 1.1           |
| remediation-factor | REMEDIATION_FACTOR | The multiplier factor in the sin(x) function for remediation message generator | 0.3           |
| min-temp           | MIN_TEMP           | The minimum temperature to start with                                          | 27.0          |
| min-hum            | MIN_HUM            | The minimum humidity to start with                                             | 60.0          |
| update-frequency   | UPDATE_FREQUENCY   | The update frequency for monitoring messages in seconds                        | 2             |

You can experiment with parameters after deploy and init of a device (see `README.md` inside root folder) to simulate different scenario.

### Worker

The `worker` folder contains the Lambda function that leverage Golang concurrency to pipeline different operations over the messages received. It's triggered automatically by AWS for each new message coming from the specified topic - propagated through an environment variable. The key element is at this point:

```go
...
	var wg sync.WaitGroup
	Jobs := pipeline(
		unit(event),
		publishMetric,
		historicizeOnS3Bucket,
		persistOnDynamoDB,
	)
...
```

All these operations are compelted in a concurrent way. Have a look at [this blog post](https://madeddu.xyz/posts/go-function-composition/) for more information around function composition in Golang.

### Remediation

The `remediaton` folder contains the Lambda function that simulate some logic over the dynamo stream to send a remediation message: this message actually change a multiplier factor to simulate the strecth of the function producing a message. More complex logic could be build on top of the information stored in dynamo.