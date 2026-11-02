package natsbus

// CheckConsumerName exposes the name check Subscribe makes before it uses the
// stream, so the tests can prove every generated name passes it without a broker.
var CheckConsumerName = checkConsumerName
