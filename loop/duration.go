package loop

import (
	"fmt"
	"time"
)

// Duration 是配置里的时长字段,YAML 里写 "30s"/"5m" 这类字符串,
// 或裸数字(按秒解释)。零值表示未配置(取默认),负值表示显式关闭。
type Duration time.Duration

// UnmarshalYAML 支持 "30s" 字符串与裸数字(秒)两种写法。
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("bad duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := unmarshal(&n); err != nil {
		return fmt.Errorf("duration: expect \"30s\"-style string or number of seconds")
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// Std 返回标准库时长。
func (d Duration) Std() time.Duration { return time.Duration(d) }
