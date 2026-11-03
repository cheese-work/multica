def q(cur, name):
    cur.execute("SELECT * FROM u WHERE n = ''%s''" % name)
