package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

import java.util.List;
import java.util.Map;

/** in-toto Statement v1 model. */
public final class InToto {
    public static final String STATEMENT_TYPE = "https://in-toto.io/Statement/v1";

    private InToto() {}

    @JsonPropertyOrder({"_type", "subject", "predicateType", "predicate"})
    public record Statement(@JsonProperty("_type") String type, List<Subject> subject,
                            String predicateType, Object predicate) {
        public Statement(String subjectName, String subjectDigest,
                         String predicateType, Object predicate) {
            this(STATEMENT_TYPE,
                    List.of(new Subject(subjectName,
                            Map.of("sha256", subjectDigest.replaceFirst("^sha256:", "")))),
                    predicateType, predicate);
        }
    }

    public record Subject(String name, Map<String, String> digest) {}
}
