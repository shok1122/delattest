// パスワードマネージャ wasm モジュール
//
// 入力（POST /execute の JSON ボディ {"wasm":..., "data":[...], "args":[...]} 経由）:
//   /data/input0 : ボルト（1行1エントリ: name<TAB>username<TAB>password）。
//                  "data" の1番目（ボルトの data_id）としてマウントされる。
//                  空行と '#' 始まりの行は無視する
//   コマンドは次の優先順で決まる:
//     1. WASI argv（"args":["get","github"] のように指定。
//        argv[1] 以降を空白1個で連結して1行のコマンドとして扱う）
//     2. /data/input1（コマンド1行を登録済みデータとして "data" の2番目に指定）
//     3. どちらも無ければ "list"
//
// コマンド:
//   list                              エントリ名と username の一覧（パスワードは出力しない）
//   get <name>                        該当エントリのパスワードだけを stdout に出力
//   add <name> <username> <password>  エントリを追加した新ボルト全体を stdout に出力
//   del <name>                        エントリを除いた新ボルト全体を stdout に出力
//
// 更新系（add/del）はサンドボックスに書き込み先が無いため、新しいボルト全体を
// stdout に返す。呼び出し側はそれを POST /data で新データとして登録し、旧ボルトを
// DELETE /data/{id} で削除する（削除証明が発行される）ことで「更新」が完結する。
//
// パスワード生成コマンドは提供しない: サンドボックス内の乱数は決定的な擬似値で
// あり（SPEC §7.1）、安全な乱数源が無いため。
//
// エラーはすべて stderr に出力し、終了コード 0 で終わる（runner は終了コードが
// 0 以外だと stdout/stderr を捨てて "WASM error: exit_code(N)" だけを返すため、
// メッセージを届けるにはこうするしかない）。runner は stderr が非空なら
// "-- stdout -- / -- stderr --" 併記で応答するので、エラーの有無は stderr
// セクションの有無で判定できる。stdout には結果（一覧・パスワード・新ボルト）
// 以外を書かない。

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* エラーメッセージを stderr に出して終了（終了コードは上記の理由で 0） */
#define DIE(...) do { fprintf(stderr, __VA_ARGS__); exit(0); } while (0)

#define VAULT_PATH "/data/input0"
#define CMD_PATH "/data/input1"
#define MAX_ENTRIES 4096
#define MAX_FILE_BYTES (1 << 20) /* runner の stdout 上限 1 MiB に合わせる */

typedef struct {
    char *name;
    char *username;
    char *password;
} entry;

static char *read_all(const char *path, size_t *out_len) {
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    size_t cap = 4096, len = 0;
    char *buf = malloc(cap);
    if (!buf) {
        fclose(f);
        return NULL;
    }
    for (;;) {
        if (len == cap) {
            if (cap >= MAX_FILE_BYTES) {
                DIE("error: %s exceeds %d bytes\n", path, MAX_FILE_BYTES);
            }
            cap *= 2;
            buf = realloc(buf, cap);
            if (!buf) {
                fclose(f);
                return NULL;
            }
        }
        size_t n = fread(buf + len, 1, cap - len, f);
        if (n == 0) break;
        len += n;
    }
    fclose(f);
    buf = realloc(buf, len + 1);
    buf[len] = '\0';
    *out_len = len;
    return buf;
}

/* ボルトを行単位でパースする。text は破壊的に書き換えられる */
static size_t parse_vault(char *text, entry *entries) {
    size_t count = 0;
    int lineno = 0;
    char *save = NULL;
    for (char *line = strtok_r(text, "\n", &save); line;
         line = strtok_r(NULL, "\n", &save)) {
        lineno++;
        /* 行末の \r を許容（CRLF 対策） */
        size_t l = strlen(line);
        if (l > 0 && line[l - 1] == '\r') line[l - 1] = '\0';
        if (line[0] == '\0' || line[0] == '#') continue;

        char *fsave = NULL;
        char *name = strtok_r(line, "\t", &fsave);
        char *user = strtok_r(NULL, "\t", &fsave);
        char *pass = strtok_r(NULL, "\t", &fsave);
        if (!name || !user || !pass || strtok_r(NULL, "\t", &fsave)) {
            DIE("error: vault line %d: expected 3 tab-separated fields\n", lineno);
        }
        if (count == MAX_ENTRIES) {
            DIE("error: vault has more than %d entries\n", MAX_ENTRIES);
        }
        entries[count].name = name;
        entries[count].username = user;
        entries[count].password = pass;
        count++;
    }
    return count;
}

static entry *find_entry(entry *entries, size_t count, const char *name) {
    for (size_t i = 0; i < count; i++) {
        if (strcmp(entries[i].name, name) == 0) return &entries[i];
    }
    return NULL;
}

static void print_vault(const entry *entries, size_t count) {
    for (size_t i = 0; i < count; i++) {
        printf("%s\t%s\t%s\n", entries[i].name, entries[i].username, entries[i].password);
    }
}

static void check_field(const char *label, const char *v) {
    if (v[0] == '\0' || strpbrk(v, "\t\n")) {
        DIE("error: %s must be non-empty and contain no tab/newline\n", label);
    }
}

/* argv[1] 以降を空白1個で連結して1行のコマンドにする。
   連続する空白は保存されない点に注意（add のパスワードに影響し得る） */
static char *join_args(int argc, char **argv) {
    size_t total = 0;
    for (int i = 1; i < argc; i++) {
        total += strlen(argv[i]) + 1; /* 区切りの空白 or 終端 NUL の分 */
    }
    char *line = malloc(total);
    if (!line) {
        DIE("error: out of memory\n");
    }
    char *p = line;
    for (int i = 1; i < argc; i++) {
        size_t l = strlen(argv[i]);
        memcpy(p, argv[i], l);
        p += l;
        *p++ = (i + 1 < argc) ? ' ' : '\0';
    }
    return line;
}

int main(int argc, char **argv) {
    size_t vault_len = 0;
    char *vault_text = read_all(VAULT_PATH, &vault_len);
    if (!vault_text) {
        DIE("error: cannot open %s (vault must be the first data input)\n", VAULT_PATH);
    }

    static entry entries[MAX_ENTRIES];
    size_t count = parse_vault(vault_text, entries);

    /* コマンド: argv > /data/input1 > "list" の優先順。最初の1行だけを見る */
    char *cmd_line;
    if (argc >= 2) {
        cmd_line = join_args(argc, argv);
    } else {
        size_t cmd_len = 0;
        char *cmd_text = read_all(CMD_PATH, &cmd_len);
        cmd_line = cmd_text ? cmd_text : "list";
    }
    char *nl = strchr(cmd_line, '\n');
    if (nl) *nl = '\0';

    char *save = NULL;
    char *cmd = strtok_r(cmd_line, " ", &save);
    if (!cmd) {
        DIE("error: empty command\n");
    }

    if (strcmp(cmd, "list") == 0) {
        if (strtok_r(NULL, " ", &save)) {
            DIE("usage: list\n");
        }
        for (size_t i = 0; i < count; i++) {
            printf("%s\t%s\n", entries[i].name, entries[i].username);
        }
        return 0;
    }

    if (strcmp(cmd, "get") == 0) {
        char *name = strtok_r(NULL, " ", &save);
        if (!name || strtok_r(NULL, " ", &save)) {
            DIE("usage: get <name>\n");
        }
        entry *e = find_entry(entries, count, name);
        if (!e) {
            DIE("error: no entry named %s\n", name);
        }
        printf("%s\n", e->password);
        return 0;
    }

    if (strcmp(cmd, "add") == 0) {
        char *name = strtok_r(NULL, " ", &save);
        char *user = strtok_r(NULL, " ", &save);
        char *pass = save && *save ? save : NULL; /* パスワードは行末まで（空白可） */
        if (!name || !user || !pass) {
            DIE("usage: add <name> <username> <password>\n");
        }
        check_field("name", name);
        check_field("username", user);
        check_field("password", pass);
        if (find_entry(entries, count, name)) {
            DIE("error: entry %s already exists (del it first)\n", name);
        }
        if (count == MAX_ENTRIES) {
            DIE("error: vault is full (%d entries)\n", MAX_ENTRIES);
        }
        entries[count].name = name;
        entries[count].username = user;
        entries[count].password = pass;
        count++;
        print_vault(entries, count);
        return 0;
    }

    if (strcmp(cmd, "del") == 0) {
        char *name = strtok_r(NULL, " ", &save);
        if (!name || strtok_r(NULL, " ", &save)) {
            DIE("usage: del <name>\n");
        }
        entry *e = find_entry(entries, count, name);
        if (!e) {
            DIE("error: no entry named %s\n", name);
        }
        size_t idx = (size_t)(e - entries);
        memmove(&entries[idx], &entries[idx + 1], (count - idx - 1) * sizeof(entry));
        count--;
        print_vault(entries, count);
        return 0;
    }

    DIE("error: unknown command %s (list | get | add | del)\n", cmd);
}
