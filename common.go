package main

type Channel struct {
	GuideNumber    string `json:"GuideNumber"`
	GuideName      string `json:"GuideName"`
	VideoCodec     string `json:"VideoCodec"`
	AudioCodec     string `json:"AudioCodec"`
	HD             int    `json:"HD"`
	SignalStrength int    `json:"SignalStrength"`
	SignalQuality  int    `json:"SignalQuality"`
	URL            string `json:"URL"`
}
