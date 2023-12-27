package types

// ExperimentDetails is for collecting all the experiment-related details
type ExperimentDetails struct {
	Kind         string
	Name         string
	Namespace    string
	Duration     int
	Interval     int
	TargetNumber int32
}
