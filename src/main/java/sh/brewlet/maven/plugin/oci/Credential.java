package sh.brewlet.maven.plugin.oci;

/**
 * Registry credentials (username + password/token).
 * The password is held as a {@link String} and is never logged (see
 * {@link #toString()}); callers should avoid retaining it longer than needed.
 */
public class Credential {

    private final String username;
    private final String password;

    public Credential(String username, String password) {
        this.username = username;
        this.password = password;
    }

    public String getUsername() { return username; }

    /** Returns the password/token. Never log this value. */
    public String getPassword() { return password; }

    @Override
    public String toString() {
        return "Credential{username='" + username + "', ******";
    }
}
