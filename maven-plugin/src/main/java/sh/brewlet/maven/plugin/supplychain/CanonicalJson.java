package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.databind.json.JsonMapper;

import java.io.IOException;

/** Stable JSON serialization used for signed and content-addressed documents. */
public final class CanonicalJson {
    public static final ObjectMapper MAPPER = JsonMapper.builder()
            .enable(MapperFeature.SORT_PROPERTIES_ALPHABETICALLY)
            .enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS)
            .build();

    private CanonicalJson() {}

    public static byte[] bytes(Object value) throws IOException {
        return MAPPER.writeValueAsBytes(value);
    }
}
