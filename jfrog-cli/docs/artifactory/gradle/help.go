package gradle

const Description = "Run Gradle build."

var Usage = []string{`jfrog rt gradle "<tasks and options>" <config file path> [command options]`, `jfrog rt gradle "<tasks and options> -b path/to/build.gradle" <config file path> [command options]`}

const Arguments string =
`	tasks and options
		Tasks and options to run with gradle command.

	config file path
		Path to a configuration file generated by the "jfrog rt gradlec" command.`