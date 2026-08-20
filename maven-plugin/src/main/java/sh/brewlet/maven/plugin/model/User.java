package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * Optional UNIX user/group override for the sandbox process.
 * Maps to {@code User} in {@code src/internal/artifact/artifact.go}.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class User {

    @JsonProperty("uid")
    private int uid;

    @JsonProperty("gid")
    private int gid;

    public User() {}

    public User(int uid, int gid) {
        this.uid = uid;
        this.gid = gid;
    }

    public int getUid() { return uid; }
    public void setUid(int uid) { this.uid = uid; }

    public int getGid() { return gid; }
    public void setGid(int gid) { this.gid = gid; }
}
