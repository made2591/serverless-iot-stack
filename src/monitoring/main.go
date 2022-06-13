/*

This CLI is used to simulate two device at the same time in a simulated
environment. The environment is subjected to temperature and humidity
change.

- a device to MONITOR the surrounding environment;
- a device to TAKE SOME ACTION in the environment;

More in detail, the program send some messages to an MQTT broker in the
AWS Cloud (service IOT Core) with a fixed frequency, using a sin(x) like
function to simulate periodical (pretty deterministic) variation.
At the same time, the program listen over another topic from the same MQTT
broker for some remediation message. Whenever it receive a remediation
action, it use some basic logic to strech the function used to simulate
enrivonment variation.

The overall scope of this CLI is to provide a good monitor/actuator device
to support the scenario described in the README (root level) of this
repository.

*/
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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

// ****************************************************
// ******************* VARS & CONS ********************
// ****************************************************

var (
	err               error
	deviceId          string
	iotCoreEndpoint   string
	lastTemp          float64
	lastHum           float64
	minTemp           float64
	maxTemp           float64
	minHum            float64
	maxHum            float64
	velocity          float64
	updateFrequency   float64
	remediationFactor float64
	remediationLogic  int16
	logLevel          string
)

const (
	Monitor Action = iota
	Remediate
	DEVICE_ID               = "381938912"
	UPDATE_FREQUENCY        = 2
	VELOCITY                = 1.1
	REMEDIATION_FACTOR      = 0.3
	MIN_TEMP                = 27.0
	MIN_HUM                 = 60.0
	MONITORING_DEVICE_NAME  = "monitoring-device"
	BUILDING                = "1"
	IOT_CORE_ENDPOINT       = "CHANGE_ME"
	ROOT_CA_PATH            = "./certs/AmazonRootCA1.pem"
	DEVICE_CA_PATH          = "./certs/monitoring-device.cert.pem"
	DEVICE_PRIVATE_KEY_PATH = "./certs/monitoring-device.private.key"
)

// ****************************************************
// ********************* HELPERS **********************
// ****************************************************

// environment simulator
func environmentSimulator(y float64, x float64) float64 {
	return y * math.Sin((1.0/40.0)*x)
}

// map the integer value of an action to its corresponding value
func (d Action) String() string {
	return [...]string{"Monitor", "Remediate"}[d]
}

// create a TLS configuration object for MQTT communication
func newTLSConfig() (config *tls.Config, err error) {

	// create certpool
	certpool := x509.NewCertPool()
	pemCerts, err := ioutil.ReadFile(ROOT_CA_PATH)
	if err != nil {
		return
	}
	certpool.AppendCertsFromPEM(pemCerts)

	// load keypair
	cert, err := tls.LoadX509KeyPair(DEVICE_CA_PATH, DEVICE_PRIVATE_KEY_PATH)
	if err != nil {
		return
	}

	// create config object
	config = &tls.Config{
		RootCAs:      certpool,
		ClientAuth:   tls.NoClientCert,
		ClientCAs:    nil,
		Certificates: []tls.Certificate{cert},
	}
	return
}

// ****************************************************
// ****************** CORE FUNCTION *******************
// ****************************************************

// simulate the remediation logic in the environment
func remediationLogicSimulator(client mqtt.Client, msg mqtt.Message) {
	log.Info("Remediation logic activated...")
	log.Debugf("New remediation message in topic %s: %s\n", msg.Topic(), string(msg.Payload()))
	var iotEvent IoTEvent
	json.Unmarshal([]byte(msg.Payload()), &iotEvent)
	remediationLogic = 1
	if iotEvent.Body.Temp < lastTemp {
		remediationLogic = -1
	}
}

// prepare the simulator by setting message handling
func prepareSimulatedDevices() mqtt.Client {

	// create TLS configuration
	tlsconfig, err := newTLSConfig()
	if err != nil {
		log.Fatalf("Failed to create TLS configuration: %v", err)
	}
	opts := mqtt.NewClientOptions()
	log.Debugf("MQTT Broker endpoint tls://%s:8883", iotCoreEndpoint)
	opts.AddBroker(fmt.Sprintf("tls://%s:8883", iotCoreEndpoint))
	opts.SetClientID(MONITORING_DEVICE_NAME).SetTLSConfig(tlsconfig)

	// message handler
	opts.SetDefaultPublishHandler(remediationLogicSimulator)

	// Start the connection.
	c := mqtt.NewClient(opts)
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to create connection: %v", token.Error())
	}
	return c
}

// simulate monitoring logic using the specificied parameters
func monitoringLogicSimulator(c mqtt.Client) {
	log.Debug("Sending monitoring update...")
	x := 0.0
	for true {
		var simulatedMove, simulatedMoveWithoutRemediaton float64
		switch action := remediationLogic; action {
		case -1:
			log.Info("Simulate cool down...")
			simulatedMove = environmentSimulator(remediationFactor, x)
			simulatedMoveWithoutRemediaton = environmentSimulator(velocity, x)
			log.Debugf("Temperature comparison (no remediation: %0.4f, remediation: %0.4f)...", minTemp+simulatedMove, minTemp+simulatedMoveWithoutRemediaton)
		case 1:
			log.Info("Simulate warm up...")
			simulatedMove = environmentSimulator(remediationFactor, x)
			simulatedMoveWithoutRemediaton = environmentSimulator(velocity, x)
			log.Debugf("Temperature comparison (no remediation: %0.4f, remediation: %0.4f)...", minTemp+simulatedMove, minTemp+simulatedMoveWithoutRemediaton)
		default:
			log.Info("Simulate environment...")
			// simulate delta with provided function in given "time" (iteration)
			simulatedMove = environmentSimulator(velocity, x)
		}

		// compute new temperature and humidity, save previous
		simulatedTemp := minTemp + simulatedMove
		simulatedHum := minHum + simulatedMove
		lastTemp = simulatedTemp
		lastHum = simulatedHum

		// prepare monitoring message
		update := &IoTEvent{Body: &Information{Device: deviceId, Temp: simulatedTemp, Hum: simulatedHum, Action: Monitor.String()}}
		updateMessage, _ := json.Marshal(update)

		log.Infof("Sending %s %s update: temperature %0.4fC°, humidity %0.4f", update.Body.Device, update.Body.Action, update.Body.Temp, update.Body.Hum)
		if token := c.Publish(fmt.Sprintf("%s/building-%s", MONITORING_DEVICE_NAME, BUILDING), 1, false, updateMessage); token.Wait() && token.Error() != nil {
			log.Fatalf("Failed to send update: %v", token.Error())
		}
		x = x + 1.0
		time.Sleep(time.Second * time.Duration(updateFrequency))
	}

}

// simulate actuation logic using the specificied parameters
func remediationListener(c mqtt.Client) {
	log.Info("Listening for new remediation events...")
	if token := c.Subscribe(fmt.Sprintf("%s/remediation-%s", MONITORING_DEVICE_NAME, BUILDING), 0, nil); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to create subscription: %v", token.Error())
	}
}

// run everything
func main() {
	logLevel = "INFO"
	remediationLogic = 0

	// set logger
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)
	logLevelStr := os.Getenv("LOG_LEVEL")
	if strings.Compare(logLevelStr, "ERROR") == 0 {
		logLevel = "ERROR"
		log.SetLevel(log.ErrorLevel)
	}
	if strings.Compare(logLevelStr, "WARNING") == 0 {
		logLevel = "WARNING"
		log.SetLevel(log.WarnLevel)
	}
	if strings.Compare(logLevelStr, "DEBUG") == 0 {
		logLevel = "DEBUG"
		log.SetLevel(log.DebugLevel)
	}

	// set device ID from environment variable or default
	deviceId = os.Getenv("DEVICE_ID")
	if strings.Compare(deviceId, "") == 0 {
		deviceId = DEVICE_ID
	}
	// set IOT Broker endpoint from environment variable or default
	iotCoreEndpoint = os.Getenv("IOT_CORE_ENDPOINT")
	if strings.Compare(iotCoreEndpoint, "") == 0 {
		iotCoreEndpoint = IOT_CORE_ENDPOINT
	}
	// init velocity for environment simulation
	velocity, err = strconv.ParseFloat(os.Getenv("VELOCITY"), 64)
	if err != nil {
		velocity = VELOCITY
	}
	// init remediation factor for remediate simulation
	remediationFactor, err = strconv.ParseFloat(os.Getenv("REMEDIATION_FACTOR"), 64)
	if err != nil {
		remediationFactor = REMEDIATION_FACTOR
	}
	// init min temperature for environment simulation
	minTemp, err = strconv.ParseFloat(os.Getenv("MIN_TEMP"), 64)
	if err != nil {
		minTemp = MIN_TEMP
	}
	// init min humidity for environment simulation
	minHum, err = strconv.ParseFloat(os.Getenv("MIN_HUM"), 64)
	if err != nil {
		minHum = MIN_HUM
	}
	// init monitoring frequency update for environment simulation
	updateFrequency, err = strconv.ParseFloat(os.Getenv("UPDATE_FREQUENCY"), 64)
	if err != nil {
		updateFrequency = UPDATE_FREQUENCY
	}

	flag.StringVar(&iotCoreEndpoint, "iot-endpoint", iotCoreEndpoint, "IOT broker endpoint")
	flag.StringVar(&deviceId, "device-id", deviceId, "Device ID")
	flag.Float64Var(&minTemp, "min-temp", minTemp, "Minimum environment temperature")
	flag.Float64Var(&minHum, "min-hum", minHum, "Minimum environment relative humidity")
	flag.Float64Var(&velocity, "velocity", velocity, "Frequency update (seconds) from the environment monitoring device")
	flag.Float64Var(&updateFrequency, "update-frequency", updateFrequency, "Frequency update (seconds) from the environment monitoring device")
	flag.Float64Var(&remediationFactor, "remediation-factor", remediationFactor, "Frequency update (seconds) from the environment monitoring device")
	flag.StringVar(&logLevel, "log-level", logLevel, "Logging level")

	flag.Parse()

	if strings.Compare(logLevel, "ERROR") == 0 {
		logLevel = "ERROR"
		log.SetLevel(log.ErrorLevel)
	}
	if strings.Compare(logLevel, "WARNING") == 0 {
		logLevel = "WARNING"
		log.SetLevel(log.WarnLevel)
	}
	if strings.Compare(logLevel, "DEBUG") == 0 {
		logLevel = "DEBUG"
		log.SetLevel(log.DebugLevel)
	}

	fmt.Printf("Setup given:\n\n")
	fmt.Printf("\tiot-endpoint: **********%6s\n", iotCoreEndpoint[10:])
	fmt.Printf("\tdevice-id: %13s\n", deviceId)
	fmt.Printf("\tmin-temp: %11.2f C°\n", minTemp)
	fmt.Printf("\tmin-hum: %13.2f %%\n", minHum)
	fmt.Printf("\tvelocity: %14.1f\n", velocity)
	fmt.Printf("\tupdate-frequency: %5.1fs\n", updateFrequency)
	fmt.Printf("\tremediation-factor: %4.2f\n", remediationFactor)
	fmt.Printf("\tlog-level: %13s\n\nStarting simulation...", logLevel)
	time.Sleep(time.Second * 5)

	c := prepareSimulatedDevices()
	go monitoringLogicSimulator(c)
	go remediationListener(c)

	time.Sleep(time.Second * 10000)
	c.Disconnect(250)
}
